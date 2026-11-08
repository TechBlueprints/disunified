package unificfg

import (
	"reflect"
	"testing"
)

// The DX010's OSPF pushes of 2026-11-07: an OSPF router made for the switch
// in the UI with the routed test network in area 0 (the form's defaults,
// the default route announced, static routes redistributed as E1 and
// connected as E2), then MD5 authentication with a cost, a priority and
// short timers, then plain-text authentication, then a passive interface in
// a stub area with nothing announced or redistributed, then an NSSA.
func TestParseOSPF(t *testing.T) {
	if c := readL3(t, "system_cfg-dx010-bgp-1-enabled.txt"); c.OSPF != nil {
		t.Errorf("a push without switch.ospf parses as %+v", c.OSPF)
	}
	c := readL3(t, "system_cfg-dx010-ospf-1-defaults.txt")
	want := &OSPF{RouterID: "198.51.100.1", AnnounceDefault: true, RedistStatic: "e1", RedistConnected: "e2", Areas: []OSPFArea{{ID: "0.0.0.0", Type: "normal"}}}
	if !reflect.DeepEqual(c.OSPF, want) {
		t.Errorf("router = %+v\nwant     %+v", c.OSPF, want)
	}
	ris := c.Routing.Interfaces
	if len(ris) != 2 || ris[0].OSPF != nil {
		t.Fatalf("interfaces = %+v (the transit is in no area)", ris)
	}
	if got, want := ris[1].OSPF, (&OSPFInterface{Area: "0.0.0.0", Cost: 1, Priority: 1, Hello: 10, Dead: 40, Auth: "none"}); !reflect.DeepEqual(got, want) {
		t.Errorf("interface = %+v\nwant        %+v", got, want)
	}

	md5 := readL3(t, "system_cfg-dx010-ospf-2-md5.txt").Routing.Interfaces[1].OSPF
	if want := (&OSPFInterface{Area: "0.0.0.0", Cost: 10, Priority: 2, Hello: 5, Dead: 20, Auth: "md5", MD5Keys: []OSPFKey{{ID: 1, Key: "<hash>"}}}); !reflect.DeepEqual(md5, want) {
		t.Errorf("md5 = %+v\nwant  %+v", md5, want)
	}
	plain := readL3(t, "system_cfg-dx010-ospf-3-plaintext.txt").Routing.Interfaces[1].OSPF
	if plain.Auth != "plaintext" || plain.Password != "<hash>" || len(plain.MD5Keys) != 0 {
		t.Errorf("plaintext = %+v", plain)
	}
	c = readL3(t, "system_cfg-dx010-ospf-4-passive-stub.txt")
	if o := c.OSPF; o.AnnounceDefault || o.RedistStatic != "none" || o.RedistConnected != "none" || !reflect.DeepEqual(o.Areas, []OSPFArea{{ID: "0.0.0.1", Type: "stub"}}) {
		t.Errorf("stub router = %+v", o)
	}
	if i := c.Routing.Interfaces[1].OSPF; !i.Passive || i.Area != "0.0.0.1" || i.Auth != "none" {
		t.Errorf("passive = %+v", i)
	}
	if a := readL3(t, "system_cfg-dx010-ospf-5-nssa.txt").OSPF.Areas; !reflect.DeepEqual(a, []OSPFArea{{ID: "0.0.0.2", Type: "nssa"}}) {
		t.Errorf("nssa areas = %+v", a)
	}
	for _, f := range []string{"system_cfg-dx010-ospf-1-defaults.txt", "system_cfg-dx010-ospf-2-md5.txt", "system_cfg-dx010-ospf-3-plaintext.txt", "system_cfg-dx010-ospf-4-passive-stub.txt", "system_cfg-dx010-ospf-5-nssa.txt"} {
		c := readL3(t, f)
		if len(c.OSPF.Unmodelled) > 0 {
			t.Errorf("%s: router left %v", f, c.OSPF.Unmodelled)
		}
		for _, ri := range c.Routing.Interfaces {
			if len(ri.Unmodelled) > 0 || (ri.OSPF != nil && len(ri.OSPF.Unmodelled) > 0) {
				t.Errorf("%s: %s left %v %+v", f, ri.Name, ri.Unmodelled, ri.OSPF)
			}
		}
	}
}
