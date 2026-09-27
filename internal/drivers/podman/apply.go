package podman

import (
	"context"
	"fmt"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// nmState is NetworkManager's IPv4 configuration of the uplink's connection
// profile, as the collector read it: the profile name and the four values
// `nmcli -g ipv4.method,ipv4.addresses,ipv4.gateway,ipv4.dns` print, one
// per line.
type nmState struct {
	Conn      string
	Method    string // "auto" (DHCP), "manual" (static), "" unknown
	Addresses string // "192.0.2.10/24" (comma-separated when several)
	Gateway   string
	DNS       string // "192.0.2.1,192.0.2.2"
}

func parseNM(conn, ipv4 string) nmState {
	st := nmState{Conn: strings.TrimSpace(conn)}
	lines := strings.Split(strings.TrimRight(ipv4, "\n"), "\n")
	get := func(i int) string {
		if i < len(lines) {
			return strings.TrimSpace(lines[i])
		}
		return ""
	}
	st.Method, st.Addresses, st.Gateway, st.DNS = get(0), get(1), get(2), get(3)
	return st
}

// ApplyAddress applies the controller's IP Settings to the host's uplink
// through NetworkManager. Two guards, because this host is also the address
// the bridge reaches it by (often the host the bridge runs on):
//
//   - A static address is applied only if it is one the uplink already
//     carries: the operation is "make the lease permanent" (or re-affirm a
//     static), never "move the host", which the bridge could not verify and
//     which would take its own SSH target away.
//   - DHCP is refused. After adoption the controller no longer holds the
//     host's reservation, so a fresh lease could land anywhere; the operator
//     sets that by hand if they mean it.
//
// The write is `nmcli con mod` on the uplink's profile followed by `nmcli
// device reapply`, which applies the change to the live interface without
// bouncing it (the SSH session this runs over survives); the profile's
// method is read back to confirm. Idempotent: a profile already holding the
// setting is left alone.
func (c *Collector) ApplyAddress(ctx context.Context, d devicemodel.AddressDesired) (bool, error) {
	c.mu.Lock()
	last, nm, nic := c.last, c.nm, c.uplinkNIC
	c.mu.Unlock()
	if last == nil {
		return false, fmt.Errorf("podman: no snapshot yet")
	}
	if nm.Conn == "" || nic == "" {
		return false, fmt.Errorf("podman: the uplink's NetworkManager profile is unknown (is NetworkManager managing %q?); address left alone", nic)
	}
	if d.DHCP {
		return false, fmt.Errorf("podman: refusing DHCP for %s: the controller no longer holds this host's reservation once it is adopted, so a lease could move the host -- set DHCP on the host by hand if that is meant", nic)
	}
	if d.IP == "" || d.PrefixLen <= 0 {
		return false, fmt.Errorf("podman: static IP Settings without an address")
	}
	carries := false
	for _, a := range last.System.Addresses {
		if a.Iface == nic && a.IP == d.IP {
			carries = true
		}
	}
	if !carries {
		return false, fmt.Errorf("podman: refusing to move %s to %s: the uplink does not carry that address, and the bridge reaches this host by its current one; make the change on the host and update the bridge's ssh target", nic, d.IP)
	}
	wantAddr := fmt.Sprintf("%s/%d", d.IP, d.PrefixLen)
	wantDNS := strings.Join(d.DNS, ",")
	if nm.Method == "manual" && nm.Addresses == wantAddr && nm.Gateway == d.Gateway && nm.DNS == wantDNS {
		return false, nil
	}
	q := sshrun.ShellQuote
	mod := fmt.Sprintf("nmcli con mod %s ipv4.method manual ipv4.addresses %s ipv4.gateway %s ipv4.dns %s ipv4.ignore-auto-dns yes",
		q(nm.Conn), q(wantAddr), q(d.Gateway), q(wantDNS))
	if _, err := c.r.Run(ctx, mod, ""); err != nil {
		return false, fmt.Errorf("podman: nmcli con mod: %w", err)
	}
	if _, err := c.r.Run(ctx, "nmcli device reapply "+q(nic), ""); err != nil {
		return false, fmt.Errorf("podman: nmcli device reapply %s: %w (the profile is modified; it takes effect on the next activation)", nic, err)
	}
	out, err := c.r.Run(ctx, "nmcli -g ipv4.method con show "+q(nm.Conn), "")
	if err != nil {
		return false, fmt.Errorf("podman: read back: %w", err)
	}
	if strings.TrimSpace(out) != "manual" {
		return false, fmt.Errorf("podman: profile %q reads back ipv4.method %q after the write, want manual", nm.Conn, strings.TrimSpace(out))
	}
	c.mu.Lock()
	c.nm = nmState{Conn: nm.Conn, Method: "manual", Addresses: wantAddr, Gateway: d.Gateway, DNS: wantDNS}
	c.mu.Unlock()
	c.Log.Printf("podman %s: %s is now static %s via %s (dns %s), applied to %s without bouncing it", c.host, nm.Conn, wantAddr, d.Gateway, wantDNS, nic)
	return true, nil
}

var _ devicemodel.AddressController = (*Collector)(nil)
