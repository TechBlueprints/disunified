package informloop

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// ipv6Device records what the loop told it about the management network's
// IPv6.
type ipv6Device struct{ applied []bool }

func (d *ipv6Device) Collect(context.Context) (*devicemodel.Snapshot, error) {
	return &devicemodel.Snapshot{}, nil
}

func (d *ipv6Device) ApplyManagementIPv6(_ context.Context, on bool) (int, error) {
	d.applied = append(d.applied, on)
	return 1, nil
}

func ipv6Loop(t *testing.T, coll devicemodel.Collector, ask func(context.Context, int) (bool, error)) (*Loop, *strings.Builder) {
	t.Helper()
	desc := inform.Descriptor{MAC: "02:00:00:00:00:03", Model: "USWF07D"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}, nil, time.Now())
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), Collector: coll, CollectTimeout: time.Second, ManagementIPv6: ask})
	if err != nil {
		t.Fatal(err)
	}
	return l, &buf
}

// The DX010's push (switch.managementvlan=1): the loop asks about VLAN 1
// and hands the answer to the driver; a changed answer is logged once, an
// unreadable one leaves the device alone (no call), and a device without
// the capability is never asked about.
func TestManagementIPv6FollowsTheControllersNetwork(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-dx010-acl.txt"))
	if err != nil {
		t.Fatal(err)
	}
	push := string(b)
	var asked []int
	answer, answerErr := true, error(nil)
	ask := func(_ context.Context, vlan int) (bool, error) {
		asked = append(asked, vlan)
		return answer, answerErr
	}
	dev := &ipv6Device{}
	l, buf := ipv6Loop(t, dev, ask)
	ctx := context.Background()
	l.applyManagementIPv6(ctx, push)
	l.applyManagementIPv6(ctx, push)
	answer = false
	l.applyManagementIPv6(ctx, push)
	answerErr = errors.New("HTTP 401")
	l.applyManagementIPv6(ctx, push)
	if len(asked) != 4 || asked[0] != 1 {
		t.Errorf("asked about VLANs %v, want 1 four times", asked)
	}
	if want := []bool{true, true, false}; len(dev.applied) != len(want) || dev.applied[0] != want[0] || dev.applied[1] != want[1] || dev.applied[2] != want[2] {
		t.Errorf("driver told %v, want %v (nothing on the error)", dev.applied, want)
	}
	logged := buf.String()
	for _, s := range []string{"runs IPv6 on the management network (VLAN 1)", "runs no IPv6 on the management network (VLAN 1)", "could not be read (HTTP 401)"} {
		if strings.Count(logged, s) != 1 {
			t.Errorf("%q logged %d times:\n%s", s, strings.Count(logged, s), logged)
		}
	}

	asked = nil
	plain, _ := ipv6Loop(t, &fanDevice{}, ask)
	plain.applyManagementIPv6(ctx, push)
	if len(asked) != 0 {
		t.Errorf("a driver without ManagementIPv6Controller was asked about: %v", asked)
	}
}
