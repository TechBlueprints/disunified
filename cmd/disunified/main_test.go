package main

import (
	"reflect"
	"testing"
)

// -device-ssh-key / DUI_DEVICE_SSH_KEY is the config file's options.ssh_key
// for a device run from the flags (docs/drivers/sonic.md §10.5): the drivers
// read the same option, so a .pub selects that one agent identity there too.
func TestFlagDriverOptions(t *testing.T) {
	if got := flagDriverOptions(""); got != nil {
		t.Errorf("no key: %v, want nil (every agent key, one at a time)", got)
	}
	if got := flagDriverOptions("  "); got != nil {
		t.Errorf("blank key: %v, want nil", got)
	}
	want := map[string]string{"ssh_key": "/home/admin/.ssh/id_ed25519.pub"}
	if got := flagDriverOptions(" /home/admin/.ssh/id_ed25519.pub "); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
