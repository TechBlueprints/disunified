package proxmox

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Every port's IfName is an interface name (docs/wire-shapes.md §6): no
// whitespace, at most 15 characters, the same from one collect to the next
// -- guests (`vm100-net0`, `ct101-net0`), bond slaves (`bond0-1`), the NICs
// under nothing (`eno1`) and free slots (no name) alike, on both committed
// node captures. The label a person reads is in Name and Description.
func TestIfNamesAreInterfaceNames(t *testing.T) {
	for _, capture := range []string{"collect-node1.txt", "collect-node2.txt"} {
		c, _ := newTestCollector(t, capture)
		snap, err := c.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		first := map[int]string{}
		for _, p := range snap.Ports {
			first[p.Index] = p.IfName
			if len(p.IfName) > 15 || strings.ContainsAny(p.IfName, " \t\r\n\v\f") {
				t.Errorf("%s port %d: IfName %q is not an interface name (Name %q)", capture, p.Index, p.IfName, p.Name)
			}
			if p.Present && p.IfName == "" {
				t.Errorf("%s port %d (%s): a present port without an interface name", capture, p.Index, p.Name)
			}
		}
		again, err := c.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		next := map[int]string{}
		for _, p := range again.Ports {
			next[p.Index] = p.IfName
		}
		if !reflect.DeepEqual(first, next) {
			t.Errorf("%s: interface names changed between two collects:\n%v\n%v", capture, first, next)
		}
	}
}
