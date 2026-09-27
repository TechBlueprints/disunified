package apcbackups

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Driver presents an APC Back-UPS Pro network model through its embedded
// Network Management Card's web pages (see the package comment).
//
// Config: URL = http://<card> (the card's web UI; https is not offered on
// AOS 6 by default), username/password = the card's device or admin login
// (the same login FTP uses for config.ini). Options:
//
//	mac          the card's MAC (the "MAC Address" on its About page, or
//	             Override= in config.ini). The controller places the device
//	             by it, and the card applies an uploaded TCP/IP section only
//	             when the section names it.
//	netmask      the card's network mask, dotted (e.g. 255.255.255.0), and
//	gateway_mac  the gateway's L2 address as the segment sees it: with the
//	             mask, what the controller places the device by.
//	gateway      the default gateway to write with a static address. The
//	             controller's push for a UPS-class ("usp") device carries
//	             the address, mask and nameservers but no route, so
//	             without this a card made static keeps whatever gateway it
//	             had -- none, if it was on DHCP.
//	timeout      per-request timeout (default 15s)
//
// The card accepts a handful of concurrent web sessions; the driver logs
// in and out around every poll rather than holding one.
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "apc-backups" }

func (Driver) Describe() string {
	return "APC Back-UPS Pro network model (Gassan NMC, AOS 6) over its web pages; battery and per-outlet load, switched outlet groups, address via config.ini; verified on a BG500"
}

// Open builds the web runner and the collector; Start then reads the
// identity pages and the card's config once, so a bad login fails there.
func (Driver) Open(ctx context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
	base := strings.TrimSpace(cfg.URL)
	if base == "" {
		return nil, errors.New("apc-backups: set url: the card's web address (http://host)")
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("apc-backups: set username_env/password_env: the card's login (web and FTP)")
	}
	w := NewWeb(base, cfg.Username, cfg.Password)
	if v := cfg.Options["timeout"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("apc-backups: timeout %q: %w", v, err)
		}
		w.Timeout = d
		w.http.Timeout = d
	}
	c := NewCollector(w)
	c.Addr = w.Host
	c.MAC = strings.ToLower(strings.TrimSpace(cfg.Options["mac"]))
	c.Netmask = strings.TrimSpace(cfg.Options["netmask"])
	c.GatewayMAC = strings.ToLower(strings.TrimSpace(cfg.Options["gateway_mac"]))
	c.Gateway = strings.TrimSpace(cfg.Options["gateway"])
	return c, nil
}

// Compile-time contract: what this driver is, and deliberately is not (no
// port control, no reboot of the UPS itself -- the card's "Turn Off UPS"
// form is never posted).
var (
	_ devicemodel.Driver            = Driver{}
	_ devicemodel.Device            = (*Collector)(nil)
	_ devicemodel.Capable           = (*Collector)(nil)
	_ devicemodel.OutletController  = (*Collector)(nil)
	_ devicemodel.OutletCycler      = (*Collector)(nil)
	_ devicemodel.OutletPlanner     = (*Collector)(nil)
	_ devicemodel.AddressController = (*Collector)(nil)
)
