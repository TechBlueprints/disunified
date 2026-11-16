package unificfg

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
)

// What the bridge writes to disk or to its log never carries a secret the
// controller pushed (docs/install.md, "What the bridge keeps on disk"). The
// rule is the two tables below, applied by key: the reply log
// (informloop's recorder), payload-last.json and the log writer
// (internal/logredact) all go through them. The driver still receives every
// real value in memory, and the state file (device.json) keeps the applied
// push unredacted, because a restart applies it again.
//
// A redacted value keeps its line and its shape, so a redacted reply log
// still parses, replays and scrubs:
//   - an empty value stays empty (no secret is not the same as a secret);
//   - a value of 32 hex digits (an authkey, unifi.key) becomes
//     RedactedHex, which still works as a key in a replay;
//   - a BGP file (switch.bgp.N.raw_config_base64) keeps every line but
//     the secret at the end of a password line (RedactFRR);
//   - anything else becomes Redacted.
//
// internal/unificfg/secrets_test.go fails when a key in a committed fixture
// looks like a secret and neither table below says what it is.

const (
	// Redacted stands for a pushed secret in what the bridge writes.
	Redacted = "<redacted>"
	// RedactedHex stands for a pushed 32-hex-digit key (authkey, unifi.key,
	// switch.wevent.key, x_authkey): it is not the default key, so a
	// replayed mgmt_cfg still adopts.
	RedactedHex = "00000000000000000000000000000000"
)

// secretRule names the system_cfg / mgmt_cfg keys whose values are secret.
// The pattern is matched against the whole key.
type secretRule struct {
	re  *regexp.Regexp
	frr bool   // the value is a base64 FRR file: redact inside it
	why string // where it was seen
}

func rule(pattern, why string) secretRule {
	return secretRule{re: regexp.MustCompile("^(?:" + pattern + ")$"), why: why}
}

var cfgSecrets = []secretRule{
	rule(`authkey`, "mgmt_cfg: the adopted device's inform key (every adoption)"),
	rule(`unifi\.key`, "system_cfg: the same key, in every push"),
	rule(`switch\.wevent\.key`, "system_cfg: the switch event channel's key, in every switch push"),
	rule(`users\.\d+\.password`, "system_cfg: the device login's password hash, in every push"),
	rule(`switch\.radius\.[a-z]+\.\d+\.secret`, "system_cfg: a RADIUS server's shared secret (802.1X, 11.0.99)"),
	rule(`switch\.snmp\.community\.[^.]+\.name`, "system_cfg: the SNMP community (10.6.106, system_cfg-snmp-community.txt)"),
	rule(`switch\.routing_interface\.\d+\.ospf\.password`, "system_cfg: OSPF plain-text authentication key (10.6.109)"),
	rule(`switch\.routing_interface\.\d+\.ospf\.md5_keys\.\d+\.password`, "system_cfg: an OSPF MD5 key (10.6.109)"),
	rule(`nutserver\.password`, "system_cfg: the NUT server's login password (UPS)"),
	{re: regexp.MustCompile(`^switch\.bgp\.\d+\.raw_config_base64$`), frr: true, why: "system_cfg: the uploaded FRR file; neighbour passwords live in it (10.6.109)"},
	// Not captured, by the same names: any key whose last segment names a
	// secret (SNMPv3 credentials, a future RADIUS accounting secret, a PSK).
	rule(`.*\.(?:x_)?(?:password|passphrase|secret|psk|authkey|key)|.*_(?:password|passphrase|secret|key)`, "not captured: by name"),
}

// cfgNotSecret are keys that look like secrets and are not: the coverage
// test needs each one named, with its reason.
var cfgNotSecret = map[string]string{
	`sshd\.auth\.passwd`: "a flag: password login enabled/disabled",
	`sshd\.auth\.key\.\d+\.(?:value|type|status|name|comment)`:  "the controller's SSH public keys (the comment is scrubbed for privacy, not secrecy)",
	`switch\.radius\.[a-z]+\.\d+\.(?:ip|port)`:                  "where the RADIUS server is",
	`switch\.routing_interface\.\d+\.ospf\.authentication_type`: "off/plaintext/md5",
	`switch\.routing_interface\.\d+\.ospf\.passive`:             "a flag",
	`switch\.routing_interface\.\d+\.ospf\.md5_keys\.\d+\.id`:   "the MD5 key's number",
	`switch\.bgp\.\d+\.enabled`:                                 "a flag",
	`nutserver\.credential`:                                     "a flag: login required",
	`nutserver\.username`:                                       "the NUT login's name",
	`users\.(?:\d+\.(?:name|shell|status)|status)`:              "the device login's name and shell",
}

// CfgKeySecret reports whether a system_cfg / mgmt_cfg key's value is a
// secret.
func CfgKeySecret(key string) bool {
	_, ok := cfgRule(key)
	return ok
}

func cfgRule(key string) (secretRule, bool) {
	for _, r := range cfgSecrets {
		if r.re.MatchString(key) {
			return r, true
		}
	}
	return secretRule{}, false
}

// cfgKeyNotSecret reports whether cfgNotSecret names key.
func cfgKeyNotSecret(key string) bool {
	for p := range cfgNotSecret {
		if regexp.MustCompile("^(?:" + p + ")$").MatchString(key) {
			return true
		}
	}
	return false
}

// jsonSecrets are the JSON fields of a reply (or of the bridge's own
// inform, payload-last.json) whose values are secret, at any depth.
var jsonSecrets = map[string]string{
	"authkey":     "a set-adopt / mgmt_cfg key field",
	"x_authkey":   "the bridge's own inform (payload-last.json)",
	"password":    "by name",
	"x_password":  "by name",
	"secret":      "by name",
	"x_secret":    "by name",
	"credential":  "by name: a TURN credential",
	"username":    "the TURN username of build-ssh-session (10.6.106); the scrub treats it as a secret",
	"_authkey":    "a real USP-PDU-Pro's inform (10.6.106); the bridge sends none",
	"guest_token": "a real switch's inform (10.6.106 to 11.0.99); the bridge sends none",
}

// jsonNotSecret are reply fields that look like secrets and are not.
var jsonNotSecret = map[string]string{
	"md5sum":  "the checksum of an upgrade's firmware image",
	"hash_id": "the device's hashed MAC, public in every inform",
}

// JSONKeySecret reports whether a JSON field of a reply is secret.
func JSONKeySecret(key string) bool { _, ok := jsonSecrets[key]; return ok }

// cfgTextFields are the JSON fields whose string value is key=value text
// that RedactCfg handles.
func cfgTextField(key string) bool {
	return key == "system_cfg" || key == "mgmt_cfg" || strings.HasSuffix(key, "_cfg")
}

// RedactCfg returns system_cfg / mgmt_cfg text with every secret value
// replaced, every line kept, and the values it replaced.
func RedactCfg(text string) (string, []string) {
	if text == "" {
		return text, nil
	}
	var secrets []string
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(k), "#") {
			continue
		}
		r, secret := cfgRule(strings.TrimSpace(k))
		if !secret {
			continue
		}
		val := strings.TrimRight(v, "\r")
		cr := v[len(val):]
		var red string
		var found []string
		if r.frr {
			red, found = redactFRRBase64(val)
		} else {
			red, found = redactValue(val)
		}
		secrets = append(secrets, found...)
		lines[i] = k + "=" + red + cr
	}
	return strings.Join(lines, "\n"), secrets
}

var hex32 = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

func redactValue(v string) (string, []string) {
	switch {
	case v == "", v == Redacted, v == RedactedHex:
		return v, nil
	case hex32.MatchString(v):
		return RedactedHex, []string{v}
	}
	return Redacted, []string{v}
}

// frrSecret is a line of an FRR file whose end is a secret: `neighbor X
// password LINE`, `password LINE`, `enable password LINE`, `ip ospf
// authentication-key KEY`, `ip ospf message-digest-key N md5 KEY`,
// `key-string LINE`. FRR takes the rest of the line as the secret.
var frrSecret = regexp.MustCompile(`^(.*?\b(?:password|authentication-key|key-string|md5))[ \t]+(\S.*?)[ \t]*$`)

// RedactFRR replaces the secret at the end of each password line of an FRR
// configuration, keeping every other line byte for byte.
func RedactFRR(text string) (string, []string) {
	var secrets []string
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		body := strings.TrimRight(line, "\r")
		m := frrSecret.FindStringSubmatch(body)
		if m == nil || m[2] == Redacted || strings.HasPrefix(strings.TrimSpace(body), "!") {
			continue
		}
		secrets = append(secrets, m[2])
		lines[i] = m[1] + " " + Redacted + line[len(body):]
	}
	return strings.Join(lines, "\n"), secrets
}

func redactFRRBase64(v string) (string, []string) {
	if v == "" {
		return v, nil
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return redactValue(v) // not a file this table knows: all of it
	}
	red, secrets := RedactFRR(string(raw))
	if len(secrets) == 0 {
		return v, nil // unchanged, byte for byte
	}
	return base64.StdEncoding.EncodeToString([]byte(red)), append(secrets, v)
}

// RedactJSON returns a reply (or an inform) with every secret JSON field
// and every secret key of its system_cfg / mgmt_cfg text replaced, and the
// values it replaced. Input that is not a JSON object or array is returned
// as it is. Object keys come back in Go's order (sorted), as the reply log
// already writes them through json.Encoder.
func RedactJSON(raw []byte) ([]byte, []string) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
		return raw, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return raw, nil
	}
	var secrets []string
	changed := false
	v = redactAny(v, "", &secrets, &changed)
	if !changed {
		return raw, secrets
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw, secrets
	}
	return out, secrets
}

func redactAny(v any, key string, secrets *[]string, changed *bool) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = redactAny(e, k, secrets, changed)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = redactAny(e, key, secrets, changed)
		}
		return x
	case string:
		var red string
		var found []string
		switch {
		case JSONKeySecret(key):
			red, found = redactValue(x)
		case cfgTextField(key):
			red, found = RedactCfg(x)
		default:
			return x
		}
		*secrets = append(*secrets, found...)
		if red != x {
			*changed = true
		}
		return red
	}
	return v
}

// Secrets returns the secret values in system_cfg / mgmt_cfg text, for the
// log writer to mask (internal/logredact).
func Secrets(text string) []string {
	_, s := RedactCfg(text)
	return s
}
