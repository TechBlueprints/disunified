package sonic

import (
	"context"
	"fmt"
	"net"
	"time"
)

// A bridge on the switch itself (options.transport: local) has nothing to
// dial: an address move and the first STP enable prove that the switch
// still reaches the network by connecting to an address the bridge must
// keep reaching -- options.probe, which the bridge fills from the
// controller's inform address (docs/drivers/sonic-on-switch.md §3).

// probeFunc says whether the switch reaches addr (host:port, TCP) -- from
// fromIP when it is set (a candidate address for the switch itself), else
// from whatever the kernel picks. nil on the SSH transport, where a fresh
// dial is the proof.
type probeFunc func(ctx context.Context, addr, fromIP string) error

// probeTimeout bounds one probe's connect.
const probeTimeout = 5 * time.Second

// tcpProbe connects to addr, bound to fromIP when set, and closes again:
// a SYN answered from the far side over a route the new source can use.
func tcpProbe(ctx context.Context, addr, fromIP string) error {
	d := net.Dialer{Timeout: probeTimeout}
	if fromIP != "" {
		ip := net.ParseIP(fromIP)
		if ip == nil {
			return fmt.Errorf("probe source %q is not an IP address", fromIP)
		}
		d.LocalAddr = &net.TCPAddr{IP: ip}
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// probeHost is the host part of the probe address, "" when there is none.
func (c *Collector) probeHost() string {
	h, _, err := net.SplitHostPort(c.ProbeAddr)
	if err != nil {
		return ""
	}
	return h
}
