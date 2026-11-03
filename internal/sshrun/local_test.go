package sshrun

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Local's output and errors have SSH.Run's shape: stdout on success, and
// "<first six words>...: <exit>: <stderr | stdout tail>" on failure.
func TestLocalRun(t *testing.T) {
	var l Local
	ctx := context.Background()
	if out, err := l.Run(ctx, "echo one; echo two", ""); err != nil || out != "one\ntwo\n" {
		t.Errorf("stdout: %q, %v", out, err)
	}
	if out, err := l.Run(ctx, "cat", "fed on stdin\n"); err != nil || out != "fed on stdin\n" {
		t.Errorf("stdin: %q, %v", out, err)
	}
	out, err := l.Run(ctx, "echo partial; echo refused >&2; exit 3", "")
	if err == nil || out != "partial\n" {
		t.Fatalf("a failing command: %q, %v", out, err)
	}
	want := "echo partial; echo refused >&2; exit ...: exit status 3: refused | stdout: partial"
	if err.Error() != want {
		t.Errorf("failure string\n  got  %q\n  want %q", err.Error(), want)
	}
	if SessionDropped(err) {
		t.Error("a command that exited non-zero is not a dropped session")
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if out, err := l.Run(ctx, "echo still", ""); err != nil || out != "still\n" {
		t.Errorf("Close is a no-op: %q, %v", out, err)
	}
}

// A deadline kills the command (and what it started) and returns the
// context's error.
func TestLocalRunDeadline(t *testing.T) {
	var l Local
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := l.Run(ctx, "sleep 30; echo late", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run past its deadline = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the sleep was not killed: Run took %s", d)
	}
	if !SessionDropped(err) {
		t.Error("a deadline reads as a dropped session, as on SSH")
	}
	// A child the shell runs in a subshell is in the same process group.
	start = time.Now()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if _, err := l.Run(ctx2, "(sleep 30); echo late", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("subshell past its deadline = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the subshell's sleep was not killed: Run took %s", d)
	}
}

// A shell other than bash, for a host without it.
func TestLocalShell(t *testing.T) {
	l := Local{Shell: "/bin/sh"}
	if out, err := l.Run(context.Background(), "echo $0", ""); err != nil || strings.TrimSpace(out) != "/bin/sh" {
		t.Errorf("Shell /bin/sh: %q, %v", out, err)
	}
}
