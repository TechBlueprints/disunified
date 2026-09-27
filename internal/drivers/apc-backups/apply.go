package apcbackups

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// ApplyOutlets switches the switched outlet groups to match desired: a
// strict diff against the last poll, one two-step form post carrying every
// change at once (the card's form takes all groups in one go). Main groups
// are refused with one log line -- they are on whenever the UPS is, and the
// card offers no relay for them. Names are not written: the controller
// pushes none, and the card's names are the operator's labels.
func (c *Collector) ApplyOutlets(ctx context.Context, desired []devicemodel.OutletDesired) (int, error) {
	actions, changed, err := c.plan(desired)
	if err != nil || len(actions) == 0 {
		return 0, err
	}
	if _, err := c.r.Control(ctx, actions); err != nil {
		return 0, fmt.Errorf("apc-backups: %w", err)
	}
	// The card switches immediately; the next poll reads the state back.
	c.mu.Lock()
	if c.last != nil {
		for i := range c.last.Outlets {
			for _, d := range desired {
				if d.Index == c.last.Outlets[i].Index && c.last.Outlets[i].Switchable {
					c.last.Outlets[i].On = d.On
				}
			}
		}
	}
	c.mu.Unlock()
	return changed, nil
}

// PlanOutlets reports which outlets ApplyOutlets would switch.
func (c *Collector) PlanOutlets(desired []devicemodel.OutletDesired) []int {
	c.mu.Lock()
	last, rows := c.last, c.rows
	c.mu.Unlock()
	if last == nil {
		return nil
	}
	var out []int
	for _, d := range desired {
		for i, o := range last.Outlets {
			if o.Index == d.Index && i < len(rows) && rows[i].Kind == "SOG" && o.On != d.On {
				out = append(out, d.Index)
			}
		}
	}
	return out
}

// plan turns the desired state into the card's form actions.
func (c *Collector) plan(desired []devicemodel.OutletDesired) (map[int]string, int, error) {
	c.mu.Lock()
	last, rows := c.last, c.rows
	c.mu.Unlock()
	if last == nil {
		return nil, 0, fmt.Errorf("apc-backups: no snapshot yet")
	}
	actions := map[int]string{}
	changed := 0
	for _, d := range desired {
		found := false
		for i, o := range last.Outlets {
			if o.Index != d.Index {
				continue
			}
			found = true
			if i >= len(rows) {
				break
			}
			if rows[i].Kind != "SOG" {
				if o.On != d.On {
					c.warnOnce(fmt.Sprintf("mog-%d", d.Index), "outlet %d is a main outlet group: always on with the UPS, no relay; the controller's request to switch it %s is refused", d.Index, map[bool]string{true: "on", false: "off"}[d.On])
				}
				break
			}
			if o.On == d.On {
				break
			}
			code := codeOff
			if d.On {
				code = codeOn
			}
			actions[rows[i].N] = code
			changed++
		}
		if !found {
			c.warnOnce(fmt.Sprintf("unknown-%d", d.Index), "outlet %d is not one the UPS reports; ignored", d.Index)
		}
	}
	return actions, changed, nil
}

// CycleOutlet power-cycles a switched group: the card's own Reboot action
// (off, then on after the group's reboot duration).
func (c *Collector) CycleOutlet(ctx context.Context, idx int) error {
	c.mu.Lock()
	last, rows := c.last, c.rows
	c.mu.Unlock()
	if last == nil {
		return fmt.Errorf("apc-backups: no snapshot yet")
	}
	for i, o := range last.Outlets {
		if o.Index != idx {
			continue
		}
		if i >= len(rows) || rows[i].Kind != "SOG" {
			return fmt.Errorf("apc-backups: outlet %d is a main outlet group; it cannot be power-cycled", idx)
		}
		if _, err := c.r.Control(ctx, map[int]string{rows[i].N: codeReboot}); err != nil {
			return fmt.Errorf("apc-backups: %w", err)
		}
		return nil
	}
	return fmt.Errorf("apc-backups: outlet %d is not one the UPS reports", idx)
}

// ApplyAddress sets the card's own TCP/IP configuration from the
// controller's IP Settings through a partial config.ini over FTP, exactly
// as the apc-pdu driver does: the section must carry Override= with the
// card's MAC or the card ignores it; the change applies live, no reboot.
// A diff: the card's current mode and address are compared first.
func (c *Collector) ApplyAddress(ctx context.Context, d devicemodel.AddressDesired) (bool, error) {
	c.mu.Lock()
	last, dhcp := c.last, c.dhcp
	c.mu.Unlock()
	if last == nil {
		return false, fmt.Errorf("apc-backups: no snapshot yet")
	}
	if c.MAC == "" {
		return false, fmt.Errorf("apc-backups: options.mac (the card's MAC) is required to address the card: the config.ini section is ignored without it")
	}
	if d.DHCP {
		if dhcp {
			return false, nil
		}
		if err := c.r.PutConfig(ctx, tcpipConfig(c.MAC, "DHCP Only", "", "", "")); err != nil {
			return false, fmt.Errorf("apc-backups: %w", err)
		}
		c.mu.Lock()
		c.dhcp = true
		c.mu.Unlock()
		return true, nil
	}
	if d.IP == "" || d.PrefixLen <= 0 {
		return false, fmt.Errorf("apc-backups: static address without an IP and prefix length")
	}
	cur := last.System.Addresses
	if !dhcp && len(cur) == 1 && cur[0].IP == d.IP && cur[0].PrefixLen == d.PrefixLen {
		return false, nil
	}
	if err := c.r.PutConfig(ctx, tcpipConfig(c.MAC, "Manual", d.IP, maskFromPrefix(d.PrefixLen), d.Gateway)); err != nil {
		return false, fmt.Errorf("apc-backups: %w", err)
	}
	c.mu.Lock()
	c.dhcp = false
	c.mu.Unlock()
	return true, nil
}

// tcpipConfig renders the [NetworkTCP/IP] partial; mask and gateway are
// omitted when empty (a DHCP switch needs neither).
func tcpipConfig(mac, mode, ip, mask, gw string) []byte {
	var b strings.Builder
	b.WriteString("[NetworkTCP/IP]\r\n")
	b.WriteString("Override=" + strings.ToUpper(strings.ReplaceAll(mac, ":", " ")) + "\r\n")
	b.WriteString("BootMode=" + mode + "\r\n")
	if ip != "" {
		b.WriteString("SystemIP=" + ip + "\r\n")
	}
	if mask != "" {
		b.WriteString("SubnetMask=" + mask + "\r\n")
	}
	if gw != "" {
		b.WriteString("DefaultGateway=" + gw + "\r\n")
	}
	return []byte(b.String())
}

func maskFromPrefix(n int) string {
	if n < 0 || n > 32 {
		return ""
	}
	return net.IP(net.CIDRMask(n, 32)).String()
}
