package informloop

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// countingController counts the port writes the loop asks for.
type countingController struct{ calls int }

func (c *countingController) ApplyPorts(context.Context, []devicemodel.PortDesired) (int, error) {
	c.calls++
	return 0, nil
}

// A cycle whose inform goes unanswered still reconciles the device to the
// last applied push. Found on the bridge running on a SONiC switch
// (2026-11-02): a swss restart put the VLAN netdevs back at 9100, the
// switch's own informs could no longer leave it, and the reconcile that
// restores the MTU ran only after an answered inform -- so nothing did.
func TestAnUnansweredInformStillReconciles(t *testing.T) {
	applied := "switch.port.1.status=enabled\n"
	for _, tc := range []struct {
		name   string
		server func(t *testing.T) string // the inform URL
	}{
		{"connection refused", func(t *testing.T) string {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			ln.Close()
			return "http://" + addr + "/inform"
		}},
		{"connection dropped", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("no hijacker")
				}
				c, _, err := hj.Hijack()
				if err != nil {
					t.Fatal(err)
				}
				c.Close() // EOF on the client, as a dropped segment ends up
			}))
			t.Cleanup(srv.Close)
			return srv.URL + "/inform"
		}},
		{"error status", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(srv.Close)
			return srv.URL + "/inform"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &countingController{}
			st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "v1", SystemCfg: applied}
			desc := inform.Descriptor{MAC: "02:00:00:00:00:01"}
			sess := device.NewSession(desc, tc.server(t), st, nil, time.Now())
			var buf strings.Builder
			l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), Controller: ctl, AllowInitialChanges: true,
				Collector: &failingCollector{}, CollectTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			l.informOnce(context.Background())
			if ctl.calls != 1 {
				t.Fatalf("ApplyPorts called %d times after an unanswered inform, want 1; log:\n%s", ctl.calls, buf.String())
			}
		})
	}

	// A cancelled loop does not start a write on its way out.
	ctl := &countingController{}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "v1", SystemCfg: applied}
	desc := inform.Descriptor{MAC: "02:00:00:00:00:01"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: log.New(&strings.Builder{}, "", 0), Controller: ctl, AllowInitialChanges: true,
		Collector: &failingCollector{}, CollectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.informOnce(ctx)
	if ctl.calls != 0 {
		t.Errorf("ApplyPorts called %d times on a cancelled context, want 0", ctl.calls)
	}
}
