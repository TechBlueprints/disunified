package sonic

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The DX010 as captured during the OSPF run (2026-11-07): the test network
// routed on VLAN 3999, ospfd running on the driver's file.
const fixtureOSPF = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-ospf.txt"

// ospfDesired is a captured push's OSPF router as the loop hands it over
// (the loop's own translation is tested in internal/informloop).
func ospfDesired(t *testing.T, name string) devicemodel.DeviceDesired {
	t.Helper()
	cfg := ecsPush(t, name)
	d := devicemodel.DeviceDesired{OSPFSet: true}
	if cfg.OSPF == nil {
		return d
	}
	r := &devicemodel.OSPFRouter{RouterID: cfg.OSPF.RouterID, AnnounceDefault: cfg.OSPF.AnnounceDefault}
	for _, v := range []struct {
		in  string
		out *string
	}{{cfg.OSPF.RedistStatic, &r.RedistStatic}, {cfg.OSPF.RedistConnected, &r.RedistConnected}} {
		if v.in != "none" {
			*v.out = v.in
		}
	}
	for _, a := range cfg.OSPF.Areas {
		r.Areas = append(r.Areas, devicemodel.OSPFArea{ID: a.ID, Type: a.Type})
	}
	for _, ri := range cfg.Routing.Interfaces {
		if o := ri.OSPF; o != nil {
			in := devicemodel.OSPFInterface{Name: ri.Name, VLAN: ri.VLAN, Area: o.Area, Passive: o.Passive, Cost: o.Cost, Priority: o.Priority, Hello: o.Hello, Dead: o.Dead}
			if o.Auth != "none" {
				in.Auth, in.Password = o.Auth, o.Password
			}
			for _, k := range o.MD5Keys {
				in.MD5Keys = append(in.MD5Keys, devicemodel.OSPFKey{ID: k.ID, Key: k.Key})
			}
			r.Interfaces = append(r.Interfaces, in)
		}
	}
	d.OSPF = r
	return d
}

// The router as an ospfd file, from each captured push.
func TestOSPFText(t *testing.T) {
	c := &Collector{NativeVLAN: 4094}
	text, notes := c.ospfText(ospfDesired(t, "system_cfg-dx010-ospf-2-md5.txt").OSPF)
	want := "interface Vlan3999\n ip ospf area 0.0.0.0\n ip ospf cost 10\n ip ospf priority 2\n ip ospf hello-interval 5\n ip ospf dead-interval 20\n" +
		" ip ospf authentication message-digest\n ip ospf message-digest-key 1 md5 <hash>\nexit\n!\n" +
		"router ospf\n ospf router-id 198.51.100.1\n redistribute connected metric-type 2\n redistribute static metric-type 1\n default-information originate\nexit\n"
	if text != want || len(notes) != 0 {
		t.Errorf("md5 file =\n%s\nwant\n%s\nnotes %v", text, want, notes)
	}
	text, _ = c.ospfText(ospfDesired(t, "system_cfg-dx010-ospf-3-plaintext.txt").OSPF)
	if !strings.Contains(text, " ip ospf authentication\n ip ospf authentication-key <hash>\n") || strings.Contains(text, "message-digest") {
		t.Errorf("plain-text file =\n%s", text)
	}
	text, _ = c.ospfText(ospfDesired(t, "system_cfg-dx010-ospf-4-passive-stub.txt").OSPF)
	if !strings.Contains(text, " ip ospf area 0.0.0.1\n ip ospf passive\n") || !strings.Contains(text, " area 0.0.0.1 stub\n") ||
		strings.Contains(text, "redistribute") || strings.Contains(text, "default-information") || strings.Contains(text, "authentication") {
		t.Errorf("passive stub file =\n%s", text)
	}
	text, _ = c.ospfText(ospfDesired(t, "system_cfg-dx010-ospf-5-nssa.txt").OSPF)
	if !strings.Contains(text, " area 0.0.0.2 nssa\n") {
		t.Errorf("nssa file =\n%s", text)
	}
}

// The live run of 2026-11-07, step by step: ospfd restarted on the pushed
// router (its file differs from the one running), nothing on the next
// cycle, started again once it was gone (a bgp container restart),
// restarted on a change, stopped when the router was deleted.
func TestOSPFStartRestartStop(t *testing.T) {
	r := rig20261106(t, fixtureOSPF)
	ctx := context.Background()
	apply := func(step string, d devicemodel.DeviceDesired) []string {
		t.Helper()
		if _, err := r.c.Collect(ctx); err != nil {
			t.Fatal(err)
		}
		r.fr.Commands, r.fr.OSPFFiles = nil, nil
		if _, err := r.c.ApplyDevice(ctx, d); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		var out []string
		for _, c := range r.fr.Commands {
			if c == ospfStartCommand || c == ospfStopCommand {
				out = append(out, c)
			}
		}
		return out
	}
	md5 := ospfDesired(t, "system_cfg-dx010-ospf-2-md5.txt")
	if got := apply("the md5 router", md5); len(got) != 1 || got[0] != ospfStartCommand || !strings.Contains(r.fr.OSPFFiles[0], "message-digest-key 1 md5 <hash>") {
		t.Fatalf("the router wrote %q", got)
	}
	first := r.fr.OSPFFiles[0]
	if got := apply("the md5 router again", md5); len(got) != 0 {
		t.Errorf("the cycle after the start wrote %q", got)
	}

	sum := sha256Hex(first)
	r.fr.UseSection("frr_ospf", "conf "+sum) // the bgp container restarted: the file stayed, ospfd did not
	if got := apply("ospfd gone", md5); len(got) != 1 || got[0] != ospfStartCommand {
		t.Fatalf("the restore wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "ospfd is not running") {
		t.Errorf("log:\n%s", r.log.String())
	}

	if got := apply("the stub", ospfDesired(t, "system_cfg-dx010-ospf-4-passive-stub.txt")); len(got) != 1 || !strings.Contains(r.fr.OSPFFiles[0], " area 0.0.0.1 stub\n") {
		t.Fatalf("the change wrote %q", got)
	}
	gone := ospfDesired(t, "system_cfg-dx010-ospf-6-removed.txt")
	if gone.OSPF != nil {
		t.Fatalf("the removal push has a router: %+v", gone.OSPF)
	}
	if got := apply("the router deleted", gone); len(got) != 1 || got[0] != ospfStopCommand {
		t.Fatalf("the removal wrote %q", got)
	}
	if got := apply("the router deleted, again", gone); len(got) != 0 {
		t.Errorf("the cycle after the stop wrote %q", got)
	}
	if strings.Contains(r.log.String(), "<hash>") {
		t.Errorf("a key reached the log:\n%s", r.log.String())
	}
}

// With frrcfgd running the routing config (DEVICE_METADATA
// frr_mgmt_framework_config, as on the ECS), supervisord owns ospfd: the
// driver writes nothing.
func TestOSPFRefusedUnderFrrcfgd(t *testing.T) {
	r := rig20261106(t, fixtureOSPF)
	r.fr.Fixture = strings.Replace(r.fr.Fixture, `"docker_routing_config_mode": "separated"`, `"docker_routing_config_mode": "separated", "frr_mgmt_framework_config": "true"`, 1)
	ctx := context.Background()
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	r.fr.Commands = nil
	if _, err := r.c.ApplyDevice(ctx, ospfDesired(t, "system_cfg-dx010-ospf-2-md5.txt")); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.fr.Commands {
		if c == ospfStartCommand || c == ospfStopCommand {
			t.Errorf("wrote %q under frrcfgd", c)
		}
	}
	if !strings.Contains(r.log.String(), "frrcfgd") {
		t.Errorf("log:\n%s", r.log.String())
	}
}
