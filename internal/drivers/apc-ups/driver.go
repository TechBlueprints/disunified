// Package apcups presents an APC Smart-UPS as an adopted UniFi UPS, read over
// Modbus TCP from the unit's own SmartConnect Ethernet port -- no management
// card, no serial cable. Written against an SMTL1500RM3UC (UPS ID 1026,
// firmware UPS 15.5); fixtures in docs/fixtures/apc-smtl-15.5.
//
// It reports load, power, battery state and the unit's outlet groups, and
// can switch a switched group from the controller's outlet editor when the
// operator turns control on (control.outlets). The Main group is never
// commanded. The write path is unit-tested byte-exact against NUT's
// apc_modbus encoding and has not been exercised against the unit (Clint's
// decision, 2026-09-26). See docs/drivers/apc-ups.md.
package apcups

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Driver is the APC Smart-UPS (Modbus TCP) driver.
//
// Config: url = the SmartConnect port's address or DNS name (host or
// host:502; a scheme is accepted and ignored). The port has no credentials:
// the device entry needs auth: none. Prefer the name the gateway keeps for
// the unit's DHCP lease over the address: it follows the lease when the
// address changes, and the bridge reports whatever it resolves to each
// cycle (docs/drivers/apc-ups.md §7). Options:
//
//	mac      the SmartConnect port's MAC address, reported as the device's
//	         own. Modbus does not expose it; the controller already lists it
//	         as a client, and adopting a device under its own MAC replaces
//	         that client record (and any fixed-IP reservation on it). Without
//	         it a stable locally-administered address is derived from the serial.
//	netmask      the unit's network mask, dotted (e.g. 255.255.255.0), and
//	gateway_mac  the gateway's L2 address AS THE SEGMENT SEES IT (`ip neigh
//	             show <gateway>` from a host on that network) -- not the
//	             controller's listed MAC for the gateway device, which can be
//	             a different interface. Modbus carries no IP configuration,
//	             and the controller reads these reachability fields when it
//	             places a device (docs/topology-placement.md).
//	unit_id  Modbus unit id (default 1)
//	timeout  per-request response timeout (default 3s)
//
// Modbus must first be enabled on the unit's display: Configuration -> Menu
// Type -> Advanced, then Configuration -> Modbus. It ships disabled.
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "apc-ups" }

func (Driver) Describe() string {
	return "APC Smart-UPS over Modbus TCP on its SmartConnect port (read-only: load, power, battery); verified on SMTL1500RM3UC / UPS 15.5"
}

// Open builds the Modbus client and the collector; Start then reads the
// whole unit once, so an unreachable port or Modbus left disabled fails here.
func (Driver) Open(ctx context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
	addr := strings.TrimSpace(cfg.URL)
	if addr == "" {
		addr = cfg.Options["host"]
	}
	if addr == "" {
		return nil, errors.New("apc-ups: set url: the UPS's SmartConnect address")
	}
	addr = stripScheme(addr)
	if addr == "" {
		return nil, fmt.Errorf("apc-ups: could not read a host out of url %q", cfg.URL)
	}
	m := NewModbus(addr)
	if v := cfg.Options["unit_id"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 255 {
			return nil, fmt.Errorf("apc-ups: unit_id must be 0..255, got %q", v)
		}
		m.UnitID = byte(n)
	}
	if v := cfg.Options["timeout"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("apc-ups: timeout %q: %w", v, err)
		}
		m.Timeout = d
	}
	c := NewCollector(m)
	c.Addr = m.Addr
	c.MAC = strings.ToLower(strings.TrimSpace(cfg.Options["mac"]))
	c.Netmask = strings.TrimSpace(cfg.Options["netmask"])
	c.GatewayMAC = strings.ToLower(strings.TrimSpace(cfg.Options["gateway_mac"]))
	return c, nil
}

// stripScheme accepts the address with or without a scheme, path or port.
func stripScheme(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimSuffix(s, "/")
	if h, _, ok := strings.Cut(s, "/"); ok {
		s = h
	}
	return s
}

// Compile-time checks: a collect device that switches outlet groups and
// nothing else. Deliberately not a Controller (no ports to configure), not
// a Rebooter (a UPS reboot is the rack), not an AddressController (Modbus
// carries no IP configuration).
var (
	_ devicemodel.Driver           = Driver{}
	_ devicemodel.Device           = (*Collector)(nil)
	_ devicemodel.Capable          = (*Collector)(nil)
	_ devicemodel.OutletController = (*Collector)(nil)
	_ devicemodel.OutletCycler     = (*Collector)(nil)
	_ devicemodel.OutletPlanner    = (*Collector)(nil)
)
