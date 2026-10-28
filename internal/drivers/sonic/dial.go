package sonic

import (
	"context"
	"strings"

	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// The dial target follows an address move (§6.4). A move adopts the new
// address only once the switch has kept it; the target is persisted in
// sonic-managed.json (`dial`), so a restart keeps using it even when the
// config file still names the old address, and a move that was cut short
// leaves the new address as `pending_dial`. When the current target cannot
// be dialled, the other known addresses -- the pending one, the persisted
// one, the config file's -- are tried once each and the first that answers
// is adopted. Without this a half-done move left the bridge dialling a dead
// address forever (live, 2026-10-27).

// CurrentAddress implements devicemodel.AddressReporter: the address the
// driver reaches the switch at now -- the configured one, or the one an
// address move (or a fallback) adopted. The loop reports it to the
// controller as the device's address.
func (c *Collector) CurrentAddress() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Dial
}

// setPendingDial records the address a move is about to switch to.
func (c *Collector) setPendingDial(ip string) {
	c.mu.Lock()
	c.pendingDial = ip
	c.mu.Unlock()
	if err := c.saveManaged(); err != nil {
		c.Log.Printf("sonic: keeping %s: %v", managedFile, err)
	}
}

// adoptDial makes ip (reached through r) the switch's address from now on.
func (c *Collector) adoptDial(ip string, r sshrun.Runner) {
	c.mu.Lock()
	old := c.r
	c.r, c.Dial, c.savedDial, c.pendingDial = r, ip, ip, ""
	c.mu.Unlock()
	if old != nil && old != r {
		_ = old.Close()
	}
	if err := c.saveManaged(); err != nil {
		c.Log.Printf("sonic: keeping %s: %v (the new address %s is held in memory only)", managedFile, err, ip)
	}
}

// dialCandidates are the other addresses known for the switch, best first.
func (c *Collector) dialCandidates() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{c.Dial: true}
	var out []string
	for _, a := range []string{c.pendingDial, c.savedDial, c.configHost} {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// isDialError: the switch could not be reached at all (not a command that
// failed on it).
func isDialError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "ssh dial ") || strings.Contains(s, "ssh handshake ")
}

// run executes a command on the switch through the current runner; if the
// current address cannot be dialled and another is known, that one is tried
// once and adopted when it answers.
func (c *Collector) run(ctx context.Context, command, stdin string) (string, error) {
	out, err := c.runner().Run(ctx, command, stdin)
	if err == nil || !isDialError(err) {
		return out, err
	}
	c.mu.Lock()
	redial := c.Redial
	c.mu.Unlock()
	if redial == nil {
		return out, err
	}
	for _, cand := range c.dialCandidates() {
		nr := redial(cand)
		if _, perr := nr.Run(ctx, "true", ""); perr != nil {
			_ = nr.Close()
			continue
		}
		c.Log.Printf("sonic: %v; the switch answers at %s -- using that address from now on", err, cand)
		c.adoptDial(cand, nr)
		return nr.Run(ctx, command, stdin)
	}
	return out, err
}
