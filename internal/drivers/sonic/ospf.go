package sonic

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// UniFi's OSPF on a switch (docs/drivers/sonic.md §11.14). The controller
// pushes the router (switch.ospf.*) and, per routed network in an area, its
// interface settings (switch.routing_interface.N.ospf.*). The bgp container
// ships FRR's ospfd, but in SONiC's separated routing mode (bgpcfgd)
// supervisord never starts it; only frrcfgd's mode does
// (DEVICE_METADATA frr_mgmt_framework_config, Ubiquiti's choice on the ECS),
// which would move the static routes and the management default route
// onto another config daemon. So the driver runs ospfd itself: it renders
// the router into one FRR file, /etc/frr/disunified-ospfd.conf in the
// container (/etc/sonic/frr on the host, which survives a container restart
// and an image install), and starts ospfd on it. A changed file restarts
// ospfd (its adjacencies with it); a bgp container restart leaves the file
// and no ospfd, and the next reconcile starts it again; the router gone
// stops ospfd and removes the file. The file holds the authentication keys:
// it is the frr user's, mode 0600, and collect.py reads only its sha256.

const ospfConf = "/etc/frr/disunified-ospfd.conf"

// ospfStartCommand writes the file from stdin and (re)starts ospfd on it.
const ospfStartCommand = `docker exec -i bgp sh -c 'f=` + ospfConf + ` && umask 077 && cat > "$f.tmp" && chown frr:frr "$f.tmp" && mv "$f.tmp" "$f" && ` +
	`pkill -x ospfd; for i in $(seq 1 50); do pgrep -x ospfd >/dev/null || break; sleep 0.1; done; ` +
	`/usr/lib/frr/ospfd -d -A 127.0.0.1 -P 0 -f "$f"'`

// ospfStopCommand stops ospfd and removes the file.
const ospfStopCommand = `docker exec bgp sh -c 'pkill -x ospfd; rm -f ` + ospfConf + `'`

// ospfMetric is FRR's metric-type for UniFi's external type.
func ospfMetric(t string) string {
	if t == "e1" {
		return "1"
	}
	return "2"
}

// ospfText is the router as an ospfd configuration file, and notes on what
// it leaves out.
func (c *Collector) ospfText(r *devicemodel.OSPFRouter) (string, []string) {
	var b strings.Builder
	var notes []string
	areaType := map[string]string{}
	for _, a := range r.Areas {
		areaType[a.ID] = a.Type
	}
	ifaces := append([]devicemodel.OSPFInterface(nil), r.Interfaces...)
	sort.SliceStable(ifaces, func(i, j int) bool { return ifaces[i].VLAN < ifaces[j].VLAN })
	for _, in := range ifaces {
		if in.VLAN < 1 || in.VLAN > 4094 {
			notes = append(notes, fmt.Sprintf("network %q: VLAN %d is no interface; left out", in.Name, in.VLAN))
			continue
		}
		if _, ok := areaType[in.Area]; !ok {
			notes = append(notes, fmt.Sprintf("network %q: area %s is not the router's; left out", in.Name, in.Area))
			continue
		}
		fmt.Fprintf(&b, "interface %s\n ip ospf area %s\n", c.l3Iface(in.VLAN), in.Area)
		if in.Passive {
			b.WriteString(" ip ospf passive\n")
		}
		for _, kv := range []struct {
			name string
			v    int
		}{{"cost", in.Cost}, {"priority", in.Priority}, {"hello-interval", in.Hello}, {"dead-interval", in.Dead}} {
			if kv.v > 0 || (kv.name == "priority" && kv.v == 0) {
				fmt.Fprintf(&b, " ip ospf %s %d\n", kv.name, kv.v)
			}
		}
		switch in.Auth {
		case "plaintext":
			fmt.Fprintf(&b, " ip ospf authentication\n ip ospf authentication-key %s\n", in.Password)
		case "md5":
			b.WriteString(" ip ospf authentication message-digest\n")
			for _, k := range in.MD5Keys {
				fmt.Fprintf(&b, " ip ospf message-digest-key %d md5 %s\n", k.ID, k.Key)
			}
		}
		b.WriteString("exit\n!\n")
	}
	fmt.Fprintf(&b, "router ospf\n ospf router-id %s\n", r.RouterID)
	if r.RedistConnected != "" {
		fmt.Fprintf(&b, " redistribute connected metric-type %s\n", ospfMetric(r.RedistConnected))
	}
	if r.RedistStatic != "" {
		fmt.Fprintf(&b, " redistribute static metric-type %s\n", ospfMetric(r.RedistStatic))
	}
	if r.AnnounceDefault {
		b.WriteString(" default-information originate\n")
	}
	ids := make([]string, 0, len(areaType))
	for id := range areaType {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		switch t := areaType[id]; t {
		case "normal", "":
		case "stub", "nssa":
			fmt.Fprintf(&b, " area %s %s\n", id, t)
		default:
			notes = append(notes, fmt.Sprintf("area %s: type %q not translated; a normal area", id, t))
		}
	}
	b.WriteString("exit\n")
	return b.String(), notes
}

// ospfState is what collect.py's frr_ospf says: whether ospfd runs and the
// sha256 of the driver's file ("" when there is none).
type ospfState struct {
	known   bool
	running bool
	sum     string
}

func parseOSPFSection(body string, ok bool) ospfState {
	if !ok || strings.TrimSpace(body) == "error" {
		return ospfState{}
	}
	s := ospfState{known: true}
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 1 && f[0] == "running":
			s.running = true
		case len(f) == 2 && f[0] == "conf":
			s.sum = f[1]
		}
	}
	return s
}

// ospfSet records the OSPF router the push asked for, the target of every
// reconcile after it.
func (c *Collector) ospfSet(d devicemodel.DeviceDesired) {
	if !d.OSPFSet {
		return
	}
	dd := d
	c.mu.Lock()
	c.ospf = &dd
	c.mu.Unlock()
}

// applyOSPF starts, restarts or stops ospfd so that it runs the router the
// controller pushed. Returns how many writes ran.
func (c *Collector) applyOSPF(ctx context.Context) (int, error) {
	c.mu.Lock()
	st, d := c.state, c.ospf
	c.mu.Unlock()
	if st == nil || d == nil {
		return 0, nil
	}
	want := d.OSPF != nil
	if !st.bgpOn() {
		if want {
			c.warnOnce("ospf no frr", "sonic: OSPF requested, but FEATURE|bgp is not enabled (no FRR); not written")
		}
		return 0, nil
	}
	if st.meta["frr_mgmt_framework_config"] == "true" {
		if want {
			c.warnOnce("ospf frrcfgd", "sonic: OSPF: the switch runs frrcfgd (DEVICE_METADATA frr_mgmt_framework_config), whose supervisord owns ospfd; not written")
		}
		return 0, nil
	}
	if !st.ospf.known {
		if want {
			c.warnOnce("ospf unknown", "sonic: OSPF: ospfd's state could not be read; not written")
		}
		return 0, nil
	}
	if !want {
		if !st.ospf.running && st.ospf.sum == "" {
			return 0, nil
		}
		c.Log.Printf("sonic apply OSPF: stop ospfd, remove %s", ospfConf)
		if _, err := c.run(ctx, ospfStopCommand, ""); err != nil {
			return 0, fmt.Errorf("sonic apply OSPF: %w", err)
		}
		c.setOSPFState(st, ospfState{known: true})
		return 1, nil
	}
	text, notes := c.ospfText(d.OSPF)
	for _, n := range notes {
		c.warnOnce("ospf: "+n, "sonic: OSPF: %s", n)
	}
	sum := sha256Hex(text)
	if st.ospf.running && st.ospf.sum == sum {
		return 0, nil
	}
	why := "the router changed"
	switch {
	case st.ospf.sum == "":
		why = "a new router"
	case st.ospf.sum == sum:
		why = "ospfd is not running"
	}
	var nets []string
	for _, in := range d.OSPF.Interfaces {
		nets = append(nets, c.l3Iface(in.VLAN)+" area "+in.Area)
	}
	c.Log.Printf("sonic apply OSPF: (re)start ospfd (%s): router-id %s, %s", why, d.OSPF.RouterID, strings.Join(nets, ", "))
	if _, err := c.run(ctx, ospfStartCommand, text); err != nil {
		return 0, fmt.Errorf("sonic apply OSPF: %w", err)
	}
	c.setOSPFState(st, ospfState{known: true, running: true, sum: sum})
	return 1, nil
}

// setOSPFState is the state after a write, so a second apply before the
// next collect writes nothing.
func (c *Collector) setOSPFState(st *swState, o ospfState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == st {
		sim := st.clone()
		sim.ospf = o
		c.state = sim
	}
}
