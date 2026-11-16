package unificfg

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// looksSecret is what a key that might carry a secret looks like. A key in
// a committed fixture that matches it must be named by the secret tables
// (secrets.go), as a secret or as not one.
var looksSecret = regexp.MustCompile(`(?i)secret|pass|community|key|psk|cred|token|hash|md5|raw_config|auth|user`)

// fixtureTexts walks the controller fixtures: every system_cfg capture,
// the system_cfg and mgmt_cfg of every recorded reply, and the JSON
// fields of every reply and every real device's inform.
func fixtureTexts(t *testing.T) (cfgs map[string]string, jsonKeys map[string]string) {
	t.Helper()
	cfgs, jsonKeys = map[string]string{}, map[string]string{}
	root := filepath.Join("..", "..", "docs", "fixtures")
	dirs, _ := filepath.Glob(filepath.Join(root, "controller-*"))
	if len(dirs) < 3 {
		t.Fatalf("only %d controller fixture dirs", len(dirs))
	}
	var walk func(v any, src string)
	walk = func(v any, src string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if s, ok := e.(string); ok && cfgTextField(k) {
					cfgs[src+" "+k] = s
					continue
				}
				jsonKeys[k] = src
				walk(e, src)
			}
		case []any:
			for _, e := range x {
				walk(e, src)
			}
		}
	}
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*"))
		for _, f := range files {
			base := filepath.Base(f)
			switch {
			case strings.HasPrefix(base, "system_cfg") && strings.HasSuffix(base, ".txt"):
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				cfgs[f] = string(b)
			case strings.HasSuffix(base, ".ndjson"):
				fh, err := os.Open(f)
				if err != nil {
					t.Fatal(err)
				}
				sc := bufio.NewScanner(fh)
				sc.Buffer(make([]byte, 1<<20), 1<<24)
				for n := 1; sc.Scan(); n++ {
					var r struct {
						Payload json.RawMessage `json:"payload"`
					}
					if json.Unmarshal(sc.Bytes(), &r) != nil || len(r.Payload) == 0 {
						continue
					}
					var p any
					if err := json.Unmarshal(r.Payload, &p); err != nil {
						t.Fatalf("%s:%d: %v", f, n, err)
					}
					walk(p, f+":"+itoa(n))
				}
				fh.Close()
			case strings.HasPrefix(base, "inform-") && strings.HasSuffix(base, ".json"):
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				var p any
				if err := json.Unmarshal(b, &p); err != nil {
					t.Fatal(err)
				}
				walk(p, f)
			}
		}
	}
	return cfgs, jsonKeys
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func cfgKeys(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if k, _, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(strings.TrimSpace(k), "#") {
			out = append(out, strings.TrimSpace(k))
		}
	}
	return out
}

// Every key in a fixture that looks like a secret is in the table, as a
// secret or as named not to be one. A new capture with a new secret fails
// here until the table says what it is.
func TestSecretTableCoversFixtures(t *testing.T) {
	cfgs, jsonKeys := fixtureTexts(t)
	unnamed := map[string]string{}
	secrets := map[string]bool{}
	for src, text := range cfgs {
		for _, k := range cfgKeys(text) {
			switch {
			case CfgKeySecret(k):
				secrets[k] = true
				if cfgKeyNotSecret(k) {
					t.Errorf("%s is both a secret and not one", k)
				}
			case cfgKeyNotSecret(k):
			case looksSecret.MatchString(k):
				unnamed[k] = src
			}
		}
	}
	for k, src := range jsonKeys {
		_, notSecret := jsonNotSecret[k]
		if !JSONKeySecret(k) && !notSecret && looksSecret.MatchString(k) {
			unnamed["json "+k] = src
		}
	}
	for k, src := range unnamed {
		t.Errorf("%s (in %s) looks like a secret and the table in secrets.go does not say whether it is one", k, src)
	}
	// The keys the fixtures prove: each must be found as a secret.
	for _, k := range []string{"authkey", "unifi.key", "switch.wevent.key", "users.1.password", "switch.radius.auth.1.secret",
		"switch.routing_interface.2.ospf.password", "switch.routing_interface.2.ospf.md5_keys.1.password", "nutserver.password", "switch.bgp.1.raw_config_base64"} {
		if !secrets[k] {
			t.Errorf("no fixture carries %s as a secret any more", k)
		}
	}
}

// Redaction keeps every line and every key, changes only the secret
// values, is idempotent, and leaves everything the parser reads but the
// secrets as it was: a redacted reply log replays.
func TestRedactCfgKeepsShape(t *testing.T) {
	cfgs, _ := fixtureTexts(t)
	n := 0
	for src, text := range cfgs {
		red, _ := RedactCfg(text)
		if again, s := RedactCfg(red); again != red || len(s) != 0 {
			t.Errorf("%s: redacting twice changes it (%d more values)", src, len(s))
		}
		a, b := strings.Split(text, "\n"), strings.Split(red, "\n")
		if len(a) != len(b) {
			t.Fatalf("%s: %d lines, redacted %d", src, len(a), len(b))
		}
		for i := range a {
			ka, va, oka := strings.Cut(a[i], "=")
			kb, vb, okb := strings.Cut(b[i], "=")
			if ka != kb || oka != okb {
				t.Fatalf("%s line %d: key %q became %q", src, i+1, ka, kb)
			}
			if va == vb {
				continue
			}
			n++
			switch {
			case !CfgKeySecret(strings.TrimSpace(ka)):
				t.Errorf("%s: %s is not a secret and was changed", src, ka)
			case va == "":
				t.Errorf("%s: an empty %s was filled", src, ka)
			case vb != Redacted && vb != RedactedHex && !strings.HasSuffix(ka, "raw_config_base64"):
				t.Errorf("%s: %s = %q", src, ka, vb)
			}
		}
		pa, pb := Parse(text), Parse(red)
		for k := range pa.Raw {
			if !CfgKeySecret(k) && pa.Raw[k] != pb.Raw[k] {
				t.Errorf("%s: %s parsed differently after redaction", src, k)
			}
		}
	}
	if n == 0 {
		t.Fatal("no fixture value was redacted")
	}
}

// The fixtures' placeholders stand for the real secrets: none survives.
func TestRedactCfgRemovesTheSecrets(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, tc := range []struct{ file, key string }{
		{"controller-11.0.99/system_cfg-dx010-dot1x-1-enabled.txt", "switch.radius.auth.1.secret"},
		{"controller-10.6.109/system_cfg-dx010-ospf-2-md5.txt", "switch.routing_interface.2.ospf.md5_keys.1.password"},
		{"controller-10.6.109/system_cfg-dx010-ospf-3-plaintext.txt", "switch.routing_interface.2.ospf.password"},
		{"controller-10.6.106/system_cfg.txt", "users.1.password"},
		{"controller-10.6.106/system_cfg.txt", "unifi.key"},
	} {
		text := read(tc.file)
		orig := Parse(text).Raw[tc.key]
		if orig == "" {
			t.Fatalf("%s has no %s", tc.file, tc.key)
		}
		red, secrets := RedactCfg(text)
		if got := Parse(red).Raw[tc.key]; got != Redacted {
			t.Errorf("%s: %s = %q, want %q", tc.file, tc.key, got, Redacted)
		}
		found := false
		for _, s := range secrets {
			found = found || s == orig
		}
		if !found {
			t.Errorf("%s: %s's value is not among the values handed to the log mask", tc.file, tc.key)
		}
	}
}

// The adoption's mgmt_cfg, the TURN username and a real device's inform
// key fields, as the 10.6.106 reply log and informs carry them.
func TestRedactJSONReplies(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", "replies.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var mgmt, turn bool
	for sc.Scan() {
		var r struct {
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil || len(r.Payload) == 0 {
			continue
		}
		red, _ := RedactJSON(r.Payload)
		var p map[string]any
		if err := json.Unmarshal(red, &p); err != nil {
			t.Fatalf("redacted payload is not JSON: %v", err)
		}
		if m, _ := p["mgmt_cfg"].(string); strings.Contains(m, "authkey=") {
			mgmt = true
			if !strings.Contains(m, "authkey="+RedactedHex+"\n") {
				t.Errorf("mgmt_cfg after redaction: %q", m)
			}
		}
		if p["cmd"] == "build-ssh-session" {
			turn = true
			if p["username"] != Redacted || p["stun"] == Redacted {
				t.Errorf("build-ssh-session after redaction: username %v, stun %v", p["username"], p["stun"])
			}
		}
	}
	if !mgmt || !turn {
		t.Fatalf("the fixture no longer has an adoption (%v) and a build-ssh-session (%v)", mgmt, turn)
	}
	// Input that is not JSON comes back as it is.
	if out, s := RedactJSON([]byte("not json")); string(out) != "not json" || s != nil {
		t.Errorf("RedactJSON(not json) = %q, %v", out, s)
	}
}

// A BGP file (an uploaded FRR configuration): the captured files carry no
// secret and come through byte for byte; a neighbour password line, added
// to the captured file as an operator's file would carry it, loses its
// secret and nothing else.
func TestRedactFRR(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-dx010-bgp-1-enabled.txt"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	enc := Parse(text).Raw["switch.bgp.1.raw_config_base64"]
	if red, _ := RedactCfg(text); Parse(red).Raw["switch.bgp.1.raw_config_base64"] != enc {
		t.Errorf("the captured BGP file changed under redaction")
	}
	frr, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(frr), "\n")
	var neighbour string
	for _, l := range lines {
		if f := strings.Fields(l); len(f) >= 3 && f[0] == "neighbor" && f[2] == "remote-as" {
			neighbour = f[1]
			break
		}
	}
	if neighbour == "" {
		t.Fatal("the captured FRR file has no neighbour")
	}
	withPw := string(frr) + " neighbor " + neighbour + " password two words\n"
	pushed := strings.Replace(text, enc, base64.StdEncoding.EncodeToString([]byte(withPw)), 1)
	red, secrets := RedactCfg(pushed)
	got, err := base64.StdEncoding.DecodeString(Parse(red).Raw["switch.bgp.1.raw_config_base64"])
	if err != nil {
		t.Fatal(err)
	}
	want := string(frr) + " neighbor " + neighbour + " password " + Redacted + "\n"
	if string(got) != want {
		t.Errorf("redacted FRR file:\n%s\nwant:\n%s", got, want)
	}
	var pw, file bool
	for _, s := range secrets {
		pw = pw || s == "two words"
		file = file || s == base64.StdEncoding.EncodeToString([]byte(withPw))
	}
	if !pw || !file {
		t.Errorf("the log mask is not handed the password (%v) and the file (%v)", pw, file)
	}
}
