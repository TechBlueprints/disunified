package sonic

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// A switch that is still booting answers a collect long before it is a
// switch (docs/drivers/sonic.md §6.4.2). On the DX010's boot of 2026-11-14
// the bridge's unit started as soon as the swss container did, and Start's
// collect ran while portsyncd was still reading CONFIG_DB: CONFIG_DB was
// whole (32 ports), but APPL_DB had no port rows, the kernel had no VLAN
// netdevs yet, and so no address, no port up and no MAC. The loop's first
// reconcile then ran on that snapshot 40 s later. Start waits for the
// switch to finish starting instead.

// Defaults for Collector.StartWait and StartPoll.
const (
	defaultStartWait = 5 * time.Minute
	defaultStartPoll = 5 * time.Second
)

// startingWhy says why a collect's tables are those of a switch that has
// not finished starting, "" when it has:
//
//   - APPL_DB has no PORT_TABLE:PortInitDone: portsyncd has not seen every
//     port's netdev, so no port state is real yet (disunified-l2d waits for
//     the same marker);
//   - CONFIG_DB puts the address the bridge dials on an in-band interface
//     (VLAN_INTERFACE, INTERFACE, PORTCHANNEL_INTERFACE,
//     LOOPBACK_INTERFACE) and the kernel does not carry it yet: vlanmgrd
//     has not made the netdev, or intfmgrd has not added the address.
//
// An address CONFIG_DB does not hold (eth0's own MGMT_INTERFACE or DHCP,
// a dial target that does not resolve) says nothing about starting.
func startingWhy(db map[string]table, addrs []ipAddr, dial []string) string {
	var why []string
	if _, ok := db["appl_port"]["PORT_TABLE:PortInitDone"]; !ok {
		why = append(why, "APPL_DB has no PORT_TABLE:PortInitDone (swss has not finished bringing up the ports)")
	}
	kernel := map[string]bool{}
	for _, a := range addrs {
		for _, ai := range a.AddrInfo {
			if ip := net.ParseIP(ai.Local); ip != nil {
				kernel[ip.String()] = true
			}
		}
	}
	dialled := map[string]bool{}
	for _, d := range dial {
		if ip := net.ParseIP(d); ip != nil {
			dialled[ip.String()] = true
		}
	}
	var missing []string
	for k := range db["cfg_mgmt"] {
		parts := strings.Split(k, "|")
		if len(parts) != 3 {
			continue
		}
		switch parts[0] {
		case "VLAN_INTERFACE", "INTERFACE", "PORTCHANNEL_INTERFACE", "LOOPBACK_INTERFACE":
		default:
			continue
		}
		addr, _, _ := strings.Cut(parts[2], "/")
		ip := net.ParseIP(addr)
		if ip == nil || !dialled[ip.String()] || kernel[ip.String()] {
			continue
		}
		missing = append(missing, fmt.Sprintf("CONFIG_DB puts %s on %s and the kernel does not carry it yet", ip, parts[1]))
	}
	sort.Strings(missing)
	return strings.Join(append(why, missing...), "; ")
}

// startSnapshot is Start's first snapshot, of a switch that has finished
// starting: a collect that startingWhy finds early is repeated every
// StartPoll for up to StartWait, and after that Start fails (main.go
// retries it), so no snapshot from a half-started switch reaches the
// loop, the provisioner or the first reconcile.
func (c *Collector) startSnapshot(ctx context.Context) (*devicemodel.Snapshot, error) {
	began := time.Now()
	waiting := ""
	for {
		snap, err := c.Collect(ctx)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		why := c.starting
		c.mu.Unlock()
		if why == "" {
			if waiting != "" {
				c.Log.Printf("sonic: the switch has finished starting (%s after the first collect)", time.Since(began).Round(time.Second))
			}
			return snap, nil
		}
		if time.Since(began) >= c.StartWait {
			return nil, fmt.Errorf("sonic: the switch has not finished starting %s after the first collect: %s", time.Since(began).Round(time.Second), why)
		}
		if why != waiting {
			c.Log.Printf("sonic: the switch is still starting: %s; collecting again every %s (for up to %s) before the first snapshot", why, c.StartPoll, c.StartWait)
			waiting = why
		}
		t := time.NewTimer(c.StartPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
