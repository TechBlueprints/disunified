package sshrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Local runs commands on this host, for a bridge that lives on the device
// it manages. Run execs `bash -c command` with stdin on its standard
// input, as the process's own user; a driver's `sudo …` is sudo on the box.
// Output and errors have SSH.Run's shape, so a driver behaves the same on
// either runner.
type Local struct {
	Shell string // "" = /bin/bash
}

// localWaitDelay bounds the wait for the output pipes once the command has
// exited: a child it left running in the background (holding stdout) would
// otherwise hold Run until that child exits too.
const localWaitDelay = 5 * time.Second

// Run executes command with stdin on this host and returns stdout. A
// cancelled or expired context kills the command's whole process group
// (its sudo and everything under it), as SSH.Run kills the session, and
// returns ctx.Err().
func (l *Local) Run(ctx context.Context, command string, stdin string) (string, error) {
	shell := l.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = localWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // exited 0; only a background child still held the output
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%s: %w: %s", firstWords(command, 6), err, failureOutput(stderr.String(), stdout.String()))
	}
	return stdout.String(), nil
}

// Close does nothing: there is no connection to drop.
func (l *Local) Close() error { return nil }
