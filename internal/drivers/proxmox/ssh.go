package proxmox

import "github.com/TechBlueprints/disunified/internal/sshrun"

// Runner is the command transport (internal/sshrun); the fixture runner
// implements it over captures.
type Runner = sshrun.Runner

// SSH is the live transport, shared with the other host drivers.
type SSH = sshrun.SSH

// NewSSH builds the live transport for root@node.
func NewSSH(addr, user string) *SSH { return sshrun.New(addr, user) }

// truncate shortens a command for a log line.
func truncate(s string, n int) string { return sshrun.Truncate(s, n) }
