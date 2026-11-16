package apcbackups

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/apcnmc"
	"github.com/TechBlueprints/disunified/internal/drivers/reach"
)

// Collector reads the card's pages and builds the neutral snapshot.
type Collector struct {
	r Runner
	// Identity is the card's MAC (options.mac), the address dialled and
	// reported, and the netmask, gateway and gateway MAC the controller
	// places it by, as for the apc-ups driver: the pages carry no network
	// identity, so they come from the site's network list and the bridge's
	// neighbour table, or from the options (docs/drivers/apc-backups.md §8).
	// The gateway is also what a static address is written with when the
	// controller's push carries none: on the power path ("usp") it sends
	// netconf.1.* and the nameservers but no route.
	reach.Identity
	Log *log.Logger

	mu     sync.Mutex
	last   *devicemodel.Snapshot
	about  about
	ratedV int
	dhcp   bool
	cardGW string // DefaultGateway from the card's config.ini (0.0.0.0 = none)
	names  []string
	rows   []outletRow // the last poll's rows, in snapshot order
	warned map[string]bool
	syslog *apcnmc.SyslogApplier // the card's syslog servers, through config.ini
}

// NewCollector wraps a runner.
func NewCollector(r Runner) *Collector {
	c := &Collector{r: r, Log: log.Default(), warned: map[string]bool{}}
	c.Driver = "apc-backups"
	c.syslog = &apcnmc.SyslogApplier{
		Get:  r.GetConfig,
		Put:  r.PutConfig,
		Logf: func(format string, args ...any) { c.Log.Printf("apc-backups: "+format, args...) },
	}
	return c
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
	c.cardGW = ini["NetworkTCP/IP"]["DefaultGateway"]
	if c.cardGW == "0.0.0.0" {
		c.cardGW = ""
	}
	c.mu.Unlock()
	c.syslog.Load(cfg)
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
		DHCP: dhcp,
	}
	c.Fill(ctx, &snap.System)

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

// SetSiteNetworkLookup takes the site's network list (the bridge's
// controller read) for the card's netmask and gateway.
func (c *Collector) SetSiteNetworkLookup(lookup func(ctx context.Context, ip string) (devicemodel.SiteNetwork, error)) {
	c.SiteNetwork = lookup
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
