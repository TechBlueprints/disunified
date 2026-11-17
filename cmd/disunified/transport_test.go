package main

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
)

// A device with options.transport: local and neither url: nor ssh: is
// opened through its driver, not run without one (docs/drivers/
// proxmox-on-node.md §6). The driver name here is unknown on purpose, so
// the test stops at the lookup and nothing runs on this host.
func TestLocalTransportOpensTheDriver(t *testing.T) {
	o := options{driver: "no-such-driver", collectOnce: true, logger: log.New(io.Discard, "", 0),
		driverOptions: map[string]string{"transport": "local"}}
	err := runOne(context.Background(), o)
	if err == nil || strings.Contains(err.Error(), "-collect-once needs") {
		t.Fatalf("transport local was not opened through its driver: %v", err)
	}
	o.driverOptions = map[string]string{"transport": "ssh"}
	if err := runOne(context.Background(), o); err == nil || !strings.Contains(err.Error(), "-collect-once needs") {
		t.Errorf("transport ssh without a target: %v", err)
	}
}
