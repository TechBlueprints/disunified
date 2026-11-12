package device

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// A port's ifname is an interface name (docs/wire-shapes.md §6): no
// whitespace, at most 15 characters (a Linux interface name's limit), and
// the same from one collect to the next for the same port. The label a
// person reads is the port's name. Every driver's contract payload is held
// to it -- port_table[].ifname and the LLDP table's local_port_name, which
// is the same name -- built twice, through the contract tests' own
// builders, so the second build (a fresh collector on the same capture, as
// after a restart) must name every port the same.
func TestIfNamesAreInterfaceNames(t *testing.T) {
	builders := []struct {
		driver string
		build  func(*testing.T) map[string]any
	}{
		{"arista-eos", buildContractPayload},
		{"podman", buildPodmanContractPayload},
		{"proxmox", buildProxmoxContractPayload},
		{"sonic", buildSonicContractPayload},
		{"apc-pdu", buildPDUContractPayload},
		{"apc-ups", buildUPSContractPayload},
	}
	captures, err := filepath.Glob(filepath.Join("..", "..", "docs", "fixtures", "sonic-*", "collect*.txt"))
	if err != nil || len(captures) == 0 {
		t.Fatalf("no SONiC captures (%v)", err)
	}
	for _, path := range captures {
		capture := filepath.ToSlash(strings.TrimPrefix(path, filepath.Join("..", "..", "docs", "fixtures")+string(filepath.Separator)))
		builders = append(builders, struct {
			driver string
			build  func(*testing.T) map[string]any
		}{"sonic " + capture, func(t *testing.T) map[string]any { return sonicIfNamePayload(t, capture) }})
	}
	for _, b := range builders {
		t.Run(b.driver, func(t *testing.T) {
			first, again := portIfNames(t, b.build(t)), portIfNames(t, b.build(t))
			for where, name := range first {
				if len(name) > 15 || strings.ContainsAny(name, " \t\r\n\v\f") {
					t.Errorf("%s = %q: not an interface name (no whitespace, at most 15 characters)", where, name)
				}
				if again[where] != name {
					t.Errorf("%s = %q, then %q on the next build", where, name, again[where])
				}
			}
			for where := range again {
				if _, ok := first[where]; !ok {
					t.Errorf("%s appeared only on the second build", where)
				}
			}
		})
	}
}

// sonicIfNamePayload renders a SONiC capture claimed as the ECS Core. A
// 202511 capture carries its own `config` help texts; a 202505 hardware
// capture borrows the VS capture's (sonicCapturePayload).
func sonicIfNamePayload(t *testing.T, capture string) map[string]any {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", capture))
	if err != nil {
		t.Fatal(err)
	}
	c := sonic.NewCollector(fr)
	snap, err := c.Start(context.Background())
	if err != nil {
		m, _ := sonicCapturePayload(t, capture, "USWF07D")
		return m
	}
	return sonicRender(t, c, snap, "USWF07D")
}

// portIfNames collects every port interface name in a payload, keyed by
// where it was found.
func portIfNames(t *testing.T, m map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for table, key := range map[string]string{"port_table": "ifname", "lldp_table": "local_port_name"} {
		rows, _ := m[table].([]any)
		for i, r := range rows {
			e, ok := r.(map[string]any)
			if !ok {
				t.Fatalf("%s[%d] is %T", table, i, r)
			}
			v, ok := e[key]
			if !ok {
				continue
			}
			s, ok := v.(string)
			if !ok {
				t.Errorf("%s[%d].%s is %T, want a string", table, i, key, v)
				continue
			}
			idx := e["port_idx"]
			if idx == nil {
				idx = e["local_port_idx"]
			}
			out[fmt.Sprintf("%s[port %v].%s", table, idx, key)] = s
		}
	}
	return out
}
