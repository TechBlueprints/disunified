package device

import "testing"

func TestRemotePortIndex(t *testing.T) {
	for in, want := range map[string]int{
		"Port 13":        13,
		"twenty5GigE41":  42,
		"one00GigE48":    49,
		"gigE0":          1,
		"Ethernet49/1":   1, // not UniFi naming: trailing number as-is
		"eth7":           7,
		"no number here": 0,
	} {
		if got := remotePortIndex(in); got != want {
			t.Errorf("remotePortIndex(%q) = %d, want %d", in, got, want)
		}
	}
}
