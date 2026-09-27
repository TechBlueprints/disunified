package apcbackups

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Collector reads the card's pages and builds the neutral snapshot.
type Collector struct {
	r Runner
	// Addr is the card's address as dialled (host or host:port), reported
	// as the device's own; MAC, Netmask and GatewayMAC are what the
	// operator supplies for the controller to place the device by, as for
	// the apc-ups driver (the pages carry no network identity).
	Addr       string
	MAC        string
	Netmask    string
	GatewayMAC string
	// Resolve turns a host name into addresses; nil = the system resolver.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	Log     *log.Logger

	mu       sync.Mutex
	last     *devicemodel.Snapshot
	about    about
	ratedV   int
	dhcp     bool
	names    []string
	rows     []outletRow // the last poll's rows, in snapshot order
	resolved string
	warned   map[string]bool
}

// NewCollector wraps a runner.
func NewCollector(r Runner) *Collector {
	return &Collector{r: r, Log: log.Default(), warned: map[string]bool{}}
}

// Start reads the identity pages and the card's config once, then polls.
func (c *Collector) Start(ctx context.Context) (*devicemodel.Snapshot, error) {
	pages, err := c.r.Pages(ctx, "ulabout", "ulinput", "home", "uloutcfg2")
	if err != nil {
		return nil, fmt.Errorf("apc-backups: %w", err)
	}
	ab := parseAbout(pages["ulabout"])
	if ab.Model == "" || ab.RealW <= 0 {
		return nil, fmt.Errorf("apc-backups: %s answered but its About UPS page carries no model or rating; is this a Back-UPS Pro network model?", c.Addr)
	}
	cfg, err := c.r.GetConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("apc-backups: %w", err)
	}
	ini := parseINI(string(cfg))
	c.mu.Lock()
	c.about = ab
	c.ratedV = parseRatedVoltage(pages["ulinput"])
	c.dhcp = strings.HasPrefix(ini["NetworkTCP/IP"]["BootMode"], "DHCP")
	c.mu.Unlock()
	snap, err := c.build(ctx, pages)
	if err != nil {
		return nil, err
	}
	b := snap.System.Battery
	c.Log.Printf("apc-backups: %s (%s) serial %s, %s, rated %d W / %d VA, battery %s; card %s", ab.Model, ab.SKU, ab.Serial, ab.Firmware, ab.RealW, ab.ApparentVA, ab.BatteryChemistry, map[bool]string{true: "on DHCP", false: "static"}[c.dhcp])
	for i, o := range snap.Outlets {
		kind := "switched group (relay)"
		if !o.Switchable {
			kind = "main group (always on with the UPS, never commanded)"
		}
		c.Log.Printf("apc-backups: outlet %d = %q, %s, %s, %.1f W", o.Index, c.names[i], kind, map[bool]string{true: "on", false: "off"}[o.On], o.PowerW)
	}
	_ = b
	return snap, nil
}

func (c *Collector) Close() error { return nil }

// Capabilities: a UPS is not a switch; nothing is claimed.
func (c *Collector) Capabilities() devicemodel.Capabilities { return devicemodel.Capabilities{} }

// Collect fetches the two live pages (one login) and builds the snapshot.
func (c *Collector) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	pages, err := c.r.Pages(ctx, "home", "uloutcfg2")
	if err != nil {
		return nil, fmt.Errorf("apc-backups: %w", err)
	}
	return c.build(ctx, pages)
}

func (c *Collector) build(ctx context.Context, pages map[string]string) (*devicemodel.Snapshot, error) {
	h := parseHome(pages["home"])
	rows := parseOutlets(pages["uloutcfg2"])
	if len(rows) == 0 {
		return nil, fmt.Errorf("apc-backups: the Outlet Settings page carries no outlets")
	}
	c.mu.Lock()
	ab, ratedV, dhcp := c.about, c.ratedV, c.dhcp
	c.mu.Unlock()

	snap := &devicemodel.Snapshot{TakenAt: time.Now()}
	snap.System = devicemodel.System{
		Vendor: "APC", Model: ab.Model, Serial: ab.Serial, Version: strings.TrimPrefix(strings.SplitN(ab.Firmware, " /", 2)[0], "UPS "),
		MAC: strings.ToLower(c.MAC), GatewayMAC: strings.ToLower(c.GatewayMAC), DHCP: dhcp,
	}
	c.fillAddress(ctx, &snap.System)

	// --- battery, from the home page ---
	b := &devicemodel.Battery{
		ChargePct: int(h.ChargePct + 0.5), Runtime: time.Duration(h.RuntimeMin) * time.Minute,
		RealPowerRatingW: float64(ab.RealW), ApparentRatingVA: float64(ab.ApparentVA),
		InputVoltageV: h.InputV, HasInput: h.HasInput,
	}
	status := strings.ToLower(strings.Join(h.Status, " "))
	b.OnBattery = strings.Contains(status, "on battery") || strings.Contains(status, "battery power")
	b.LowBattery = strings.Contains(status, "low battery") || strings.Contains(status, "battery low")
	b.Overload = strings.Contains(status, "overload")
	b.OutputOff = strings.Contains(status, "output off") || strings.Contains(status, "ups is off")
	b.Testing = strings.Contains(status, "self-test") || strings.Contains(status, "self test")
	b.Fault = h.AlarmClass == "alarmCritical" || strings.Contains(strings.ToLower(h.BatteryLife), "replace")
	// No charging indication on the page: on mains and below full is
	// charging, as the other APC drivers derive it.
	b.Charging = !b.OnBattery && b.ChargePct < 100
	var totalW float64
	for _, r := range rows {
		totalW += r.LoadW
	}
	b.RealPowerW = totalW
	if ab.RealW > 0 {
		b.LoadPct = totalW / float64(ab.RealW) * 100
	}
	snap.System.Battery = b
	snap.System.HasPowerDraw = true
	snap.System.PowerDrawW = totalW
	snap.System.PowerBudgetW = float64(ab.RealW)

	// The card's network port is the device's one interface and its uplink.
	snap.Ports = []devicemodel.Port{{
		Index: 1, IfName: "eth0", Name: "Network", Media: devicemodel.MediaCopper1G,
		Present: true, Enabled: true, Up: true, SpeedMbps: 100, FullDuplex: true, Lanes: 1,
	}}
	snap.UplinkHint = 1

	// --- outlets: the page's order, main groups first, then switched ---
	names := make([]string, 0, len(rows))
	for i, r := range rows {
		o := devicemodel.Outlet{Index: i + 1, On: r.On, Switchable: r.Kind == "SOG", HasMetering: true, PowerW: r.LoadW}
		if ratedV > 0 {
			// The card meters watts per outlet and nothing else; the volts
			// are the rated output, the amps follow.
			o.VoltageV = float64(ratedV)
			o.CurrentA = r.LoadW / float64(ratedV)
		}
		snap.Outlets = append(snap.Outlets, o)
		names = append(names, r.Name)
	}
	c.mu.Lock()
	c.last = snap
	c.names = names
	c.rows = rows
	c.mu.Unlock()
	return snap, nil
}

// fillAddress reports the card's address: the dialled host, resolved when
// it is a name (the address follows a lease that way), with the
// operator-supplied netmask.
func (c *Collector) fillAddress(ctx context.Context, sys *devicemodel.System) {
	host := c.Addr
	if h, _, err := net.SplitHostPort(c.Addr); err == nil {
		host = h
	}
	ip := host
	if net.ParseIP(host) == nil {
		resolve := c.Resolve
		if resolve == nil {
			resolve = func(ctx context.Context, h string) ([]net.IP, error) {
				addrs, err := net.DefaultResolver.LookupIPAddr(ctx, h)
				if err != nil {
					return nil, err
				}
				out := make([]net.IP, 0, len(addrs))
				for _, a := range addrs {
					out = append(out, a.IP)
				}
				return out, nil
			}
		}
		ips, err := resolve(ctx, host)
		ip = ""
		for _, a := range ips {
			if v4 := a.To4(); v4 != nil {
				ip = v4.String()
				break
			}
		}
		if err != nil || ip == "" {
			c.Log.Printf("apc-backups: %s does not resolve to an IPv4 address (%v); address unreported this cycle", host, err)
			return
		}
		c.mu.Lock()
		if c.resolved != "" && c.resolved != ip {
			c.Log.Printf("apc-backups: %s now resolves to %s (was %s): following the lease", host, ip, c.resolved)
		}
		c.resolved = ip
		c.mu.Unlock()
	}
	sys.Addresses = []devicemodel.IfAddress{{Iface: "eth0", IP: ip, PrefixLen: prefixLen(c.Netmask)}}
}

func prefixLen(mask string) int {
	ip := net.ParseIP(mask)
	if ip == nil {
		return 0
	}
	ones, _ := net.IPMask(ip.To4()).Size()
	return ones
}

func (c *Collector) warnOnce(key, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	c.Log.Printf("apc-backups: "+format, args...)
}
