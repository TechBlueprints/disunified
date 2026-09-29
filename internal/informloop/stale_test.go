package informloop

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

type failingCollector struct{ fail bool }

func (f *failingCollector) Collect(context.Context) (*devicemodel.Snapshot, error) {
	if f.fail {
		return nil, errors.New("login: HTTP 400")
	}
	return &devicemodel.Snapshot{}, nil
}

// A device whose collection keeps failing stops being informed about after
// StaleAfter failures (the controller then shows it disconnected, as for a
// real device), and is informed again as soon as collection recovers. The
// Back-UPS reported 18 h of frozen numbers before this (2026-09-28).
func TestInformsAreWithheldWhileCollectionKeepsFailing(t *testing.T) {
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}
	desc := inform.Descriptor{MAC: "02:00:00:00:00:02", Model: "USPDA2B"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	fc := &failingCollector{fail: true}
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), Collector: fc, CollectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		l.collect(context.Background())
		if l.stale() {
			t.Fatalf("stale after %d failures; the default tolerates 4", i)
		}
	}
	l.collect(context.Background())
	if !l.stale() || !l.stale() {
		t.Fatal("five failures in a row must withhold informs")
	}
	if n := strings.Count(buf.String(), "informs withheld"); n != 1 {
		t.Errorf("the withholding is logged once, got %d", n)
	}
	fc.fail = false
	l.collect(context.Background())
	if l.stale() {
		t.Error("a successful collect must resume informs")
	}
	if !strings.Contains(buf.String(), "recovered after 5 failures") {
		t.Errorf("log: %s", buf.String())
	}
	// Opting out keeps the old behaviour.
	l2, _ := New(desc, sess, Config{Logger: log.New(&buf, "", 0), Collector: &failingCollector{fail: true}, CollectTimeout: time.Second, StaleAfter: -1})
	for i := 0; i < 20; i++ {
		l2.collect(context.Background())
	}
	if l2.stale() {
		t.Error("StaleAfter -1 must never withhold")
	}
}
