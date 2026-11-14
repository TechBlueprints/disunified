package informloop

import (
	"context"
	"fmt"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// applyManagementIPv6 tells a driver that implements
// devicemodel.ManagementIPv6Controller whether the controller runs IPv6
// on the management network, so its management interface takes the
// gateway's router advertisements as a UniFi switch's does, or drops them
// (docs/drivers/sonic.md §11.22). The push carries no IPv6 key for a
// switch; the network's own setting is read through the REST API
// (Config.ManagementIPv6). An unreadable setting leaves the device as it
// is. The setting and each error are logged when they change.
func (l *Loop) applyManagementIPv6(ctx context.Context, text string) {
	mc, ok := deviceAs[devicemodel.ManagementIPv6Controller](l)
	if !ok || l.cfg.ManagementIPv6 == nil {
		return
	}
	vlan := unificfg.Parse(text).ManagementVLAN
	if vlan == 0 {
		vlan = 1
	}
	on, err := l.cfg.ManagementIPv6(ctx, vlan)
	note := ""
	switch {
	case err != nil:
		note = fmt.Sprintf("the management network's IPv6 setting could not be read (%v); the device's IPv6 is left as it is", err)
	case on:
		note = fmt.Sprintf("the controller runs IPv6 on the management network (VLAN %d): the device takes the gateway's router advertisements", vlan)
	default:
		note = fmt.Sprintf("the controller runs no IPv6 on the management network (VLAN %d): the device takes no router advertisements", vlan)
	}
	if note != l.ipv6Noted {
		l.ipv6Noted = note
		l.cfg.Logger.Printf("[%s] IPv6: %s", l.desc.MAC, note)
	}
	if err != nil {
		return
	}
	n, err := mc.ApplyManagementIPv6(ctx, on)
	if err != nil {
		l.cfg.Logger.Printf("[%s] apply management IPv6: %v", l.desc.MAC, err)
		return
	}
	if n > 0 {
		l.cfg.Logger.Printf("[%s] applied management IPv6 (%s): %d writes", l.desc.MAC, map[bool]string{true: "on", false: "off"}[on], n)
	}
}
