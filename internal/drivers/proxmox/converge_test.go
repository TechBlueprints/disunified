package proxmox

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Two bridges, one per node, share nothing but /etc/pve
// (docs/drivers/proxmox-on-node.md §3, §5.1). The two captures hold the
// same guest set as seen from proxmox-1 and proxmox-2; each runner's tag
// writes land in both captures (FixtureRunner.Peers), as pmxcfs would
// replicate them.

type node struct {
	c   *Collector
	r   *FixtureRunner
	log *bytes.Buffer
}

// twoNodes is the cluster before any bridge ran: only VM 999 carries a tag.
func twoNodes(t *testing.T) (n1, n2 node) {
	t.Helper()
	mk := func(name string) node {
		r := &FixtureRunner{Fixture: loadFixtureRaw(t, name)}
		c := NewCollector(r)
		c.ManageLLDP = false
		var buf bytes.Buffer
		c.Log = log.New(&buf, "", 0)
		return node{c: c, r: r, log: &buf}
	}
	n1, n2 = mk("collect-node1.txt"), mk("collect-node2.txt")
	n1.r.Peers = []*FixtureRunner{n2.r}
	n2.r.Peers = []*FixtureRunner{n1.r}
	return n1, n2
}

// read is the first half of a poll: the script's output and the snapshot
// built from it, with the tag writes it asks for held back.
func (n node) read(t *testing.T) *devicemodel.Snapshot {
	t.Helper()
	out, err := n.r.Run(context.Background(), "bash -s -- vmbr0", collectScript)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := n.c.build(out, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// write is the second half: the held tag writes, returning the commands.
func (n node) write(t *testing.T) []string {
	t.Helper()
	before := len(n.r.Commands)
	n.c.mu.Lock()
	retags := n.c.pendingTags
	n.c.pendingTags = nil
	n.c.mu.Unlock()
	n.c.writeTags(context.Background(), retags)
	var out []string
	for _, cmd := range n.r.Commands[before:] {
		if strings.Contains(cmd, " --tags ") {
			out = append(out, cmd)
		}
	}
	return out
}

// round is one poll on each node, both reading before either writes (the
// worst interleaving: neither sees the other's writes of this round).
func round(t *testing.T, n1, n2 node) (s1, s2 *devicemodel.Snapshot, w1, w2 []string) {
	t.Helper()
	s1, s2 = n1.read(t), n2.read(t)
	return s1, s2, n1.write(t), n2.write(t)
}

var guestIf = regexp.MustCompile(`^(vm|ct)(\d+)-net\d+$`)

// guestPorts maps each guest NIC to its port, failing on a port shown twice.
func guestPorts(t *testing.T, who string, snap *devicemodel.Snapshot) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, p := range snap.Ports {
		if !guestIf.MatchString(p.IfName) {
			continue
		}
		if prev, dup := out[p.IfName]; dup {
			t.Fatalf("%s: %s shown at ports %d and %d", who, p.IfName, prev, p.Index)
		}
		out[p.IfName] = p.Index
	}
	return out
}

func samePorts(t *testing.T, a, b map[string]int) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("the nodes show %d and %d guest NICs", len(a), len(b))
	}
	for k, v := range a {
		if b[k] != v {
			t.Errorf("%s: port %d on proxmox-1, %d on proxmox-2", k, v, b[k])
		}
	}
}

// guestTags is the tags line of a guest's config in a capture.
func guestTags(t *testing.T, r *FixtureRunner, vmid int) string {
	t.Helper()
	nics, _ := parseGuestConfigs(sections(r.Fixture)["qemu"], "qemu")
	for _, n := range nics {
		if n.VMID == vmid {
			return strings.Join(n.Tags, ";")
		}
	}
	t.Fatalf("VM %d not in the capture", vmid)
	return ""
}

// setTags replaces a guest's tags in the cluster (both captures), as a
// clone or a restore would carry them.
func setTags(n1, n2 node, vmid int, tags string) {
	for _, r := range []*FixtureRunner{n1.r, n2.r} {
		r.applyTags("qm", fmt.Sprint(vmid), tags)
	}
}

func TestTwoWritersConverge(t *testing.T) {
	n1, n2 := twoNodes(t)

	// Both read the untagged cluster before either writes.
	s1, s2, w1, w2 := round(t, n1, n2)
	p1, p2 := guestPorts(t, "proxmox-1", s1), guestPorts(t, "proxmox-2", s2)
	samePorts(t, p1, p2)
	if len(w1) == 0 || len(w2) == 0 {
		t.Fatalf("each node should tag its own untagged guests: proxmox-1 %v, proxmox-2 %v", w1, w2)
	}
	owner := guestOwners(t, n1.r)
	for who, cmds := range map[string][]string{"proxmox-1": w1, "proxmox-2": w2} {
		for _, cmd := range cmds {
			id := tagWrite.FindStringSubmatch(cmd)
			if id == nil {
				t.Fatalf("%s: unexpected tag command %q", who, cmd)
			}
			if owner[id[2]] != who {
				t.Errorf("%s wrote the tags of VM %s, which lives on %s: %s", who, id[2], owner[id[2]], cmd)
			}
		}
	}
	// What each wrote is the port both show.
	for _, cmd := range append(append([]string{}, w1...), w2...) {
		m := tagWrite.FindStringSubmatch(cmd)
		for _, tag := range strings.Split(m[3], ";") {
			cl, ok := parseClaim(tag)
			if !ok {
				continue
			}
			nic := fmt.Sprintf("vm%s-net%d", m[2], cl.NIC)
			if p1[nic] != cl.Port {
				t.Errorf("%s was tagged port %d and is shown at %d", nic, cl.Port, p1[nic])
			}
		}
	}

	// The second poll writes nothing on either side and shows the same.
	s1, s2, w1, w2 = round(t, n1, n2)
	if len(w1)+len(w2) != 0 {
		t.Fatalf("second poll wrote tags: proxmox-1 %v, proxmox-2 %v", w1, w2)
	}
	samePorts(t, p1, guestPorts(t, "proxmox-1", s1))
	samePorts(t, p1, guestPorts(t, "proxmox-2", s2))

	// A forced duplicate whose loser lives on proxmox-1: VM 119 is given
	// VM 100's tag (a clone of it would carry it). VM 100 is lower and
	// keeps the port; only proxmox-1 writes, once.
	// And one whose loser lives on proxmox-2: VM 113 carries VM 101's.
	for _, tc := range []struct {
		winner, loser int
		loserNode     string
	}{{100, 119, "proxmox-1"}, {101, 113, "proxmox-2"}} {
		t.Run(fmt.Sprintf("dup-%d-%d", tc.winner, tc.loser), func(t *testing.T) {
			win := guestTags(t, n1.r, tc.winner)
			setTags(n1, n2, tc.loser, win)
			s1, s2, w1, w2 := round(t, n1, n2)
			g1, g2 := guestPorts(t, "proxmox-1", s1), guestPorts(t, "proxmox-2", s2)
			samePorts(t, g1, g2)
			wNode := fmt.Sprintf("vm%d-net0", tc.winner)
			if g1[wNode] != p1[wNode] {
				t.Errorf("VM %d moved from %d to %d", tc.winner, p1[wNode], g1[wNode])
			}
			loserWrites, otherWrites := w1, w2
			if tc.loserNode == "proxmox-2" {
				loserWrites, otherWrites = w2, w1
			}
			if len(otherWrites) != 0 {
				t.Errorf("the winner's side wrote: %v", otherWrites)
			}
			if len(loserWrites) != 1 || !strings.HasPrefix(loserWrites[0], fmt.Sprintf("qm set %d --tags ", tc.loser)) {
				t.Fatalf("%s wrote %v, want one rewrite of VM %d", tc.loserNode, loserWrites, tc.loser)
			}
			for who, buf := range map[string]*bytes.Buffer{"proxmox-1": n1.log, "proxmox-2": n2.log} {
				line := fmt.Sprintf("port %d: VM %d keeps it", g1[wNode], tc.winner)
				if c := strings.Count(buf.String(), line); c != 1 {
					t.Errorf("%s logged %q %d times, want once:\n%s", who, line, c, buf.String())
				}
			}
			// Settled: nothing more on either side, and the same view.
			s1, s2, w1, w2 = round(t, n1, n2)
			if len(w1)+len(w2) != 0 {
				t.Fatalf("after the duplicate: proxmox-1 %v, proxmox-2 %v", w1, w2)
			}
			samePorts(t, g1, guestPorts(t, "proxmox-1", s1))
			samePorts(t, g1, guestPorts(t, "proxmox-2", s2))
		})
	}
}

// guestOwners maps each VMID to the node its config lives under.
func guestOwners(t *testing.T, r *FixtureRunner) map[string]string {
	t.Helper()
	out := map[string]string{}
	sec := sections(r.Fixture)
	for _, kind := range []string{"qemu", "lxc"} {
		nics, _ := parseGuestConfigs(sec[kind], kind)
		for _, n := range nics {
			out[fmt.Sprint(n.VMID)] = n.Node
		}
	}
	if len(out) == 0 {
		t.Fatal("no guests in the capture")
	}
	return out
}
