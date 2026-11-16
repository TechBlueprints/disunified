package logredact

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// A driver's error that echoes a command with the push's RADIUS secret in
// it reaches the log with the secret masked. The capture's <secret> stands
// for the real one.
func TestWriterMasksPushedSecrets(t *testing.T) {
	Reset()
	defer Reset()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "system_cfg-dx010-dot1x-1-enabled.txt"))
	if err != nil {
		t.Fatal(err)
	}
	Remember(unificfg.Secrets(string(b))...)
	var out strings.Builder
	l := log.New(Writer(&out), "[dx010] ", 0)
	l.Printf("sonic apply 802.1X: printf '%%s\\n' %s | sudo tee /etc/hostapd.conf: exit status 1", "<secret>")
	if got := out.String(); strings.Contains(got, "<secret>") || !strings.Contains(got, mask) {
		t.Errorf("log line = %q", got)
	}
	if mask != unificfg.Redacted {
		t.Errorf("the log mask %q is not unificfg.Redacted %q", mask, unificfg.Redacted)
	}
}

// Short values, the placeholders themselves and the zero key are never
// masked; the longest value is masked whole; the cap drops the oldest.
func TestRememberRules(t *testing.T) {
	Reset()
	defer Reset()
	Remember("abc", unificfg.Redacted, unificfg.RedactedHex, "", "longsecret", "longsecret-and-more")
	if got := String("abc longsecret-and-more longsecret 00000000000000000000000000000000"); got != "abc <redacted> <redacted> 00000000000000000000000000000000" {
		t.Errorf("String = %q", got)
	}
	for i := 0; i < MaxValues+5; i++ {
		Remember("value-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + itoa(i))
	}
	mu.RLock()
	n, first := len(order), known["longsecret"]
	mu.RUnlock()
	if n != MaxValues || first {
		t.Errorf("kept %d values (first still known: %v), want %d and the oldest gone", n, first, MaxValues)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
