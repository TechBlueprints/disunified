package apcnmc

import (
	"strings"
)

// section returns a section's keys and values in file order, the name
// matched without regard to case, and the name as the file spells it.
// Comment lines (;) are skipped.
func section(cfg []byte, name string) (string, [][2]string, bool) {
	var (
		found string
		keys  [][2]string
		in    bool
	)
	for _, line := range strings.Split(string(cfg), "\n") {
		t := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if t == "" || strings.HasPrefix(t, ";") {
			continue
		}
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			if in {
				break
			}
			if s := t[1 : len(t)-1]; strings.EqualFold(s, name) {
				found, in = s, true
			}
			continue
		}
		if !in {
			continue
		}
		if k, v, ok := strings.Cut(t, "="); ok {
			keys = append(keys, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
		}
	}
	return found, keys, in
}

// Merge applies a partial config.ini to a whole one: each key the partial
// names replaces that key's value in the same section (matched without regard
// to case). Keys the whole file does not have are not added. It is how the
// drivers' fixture runners model the card taking an upload, so the next read
// of the file sees it, as on the card.
func Merge(cfg, partial []byte) []byte {
	set := map[string]map[string]string{}
	cur := ""
	for _, line := range strings.Split(string(partial), "\n") {
		t := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			cur = strings.ToLower(t[1 : len(t)-1])
			if set[cur] == nil {
				set[cur] = map[string]string{}
			}
			continue
		}
		if k, v, ok := strings.Cut(t, "="); ok && cur != "" {
			set[cur][strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	lines := strings.Split(string(cfg), "\n")
	cur = ""
	for i, line := range lines {
		cr := strings.HasSuffix(line, "\r")
		t := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			cur = strings.ToLower(t[1 : len(t)-1])
			continue
		}
		k, _, ok := strings.Cut(t, "=")
		if !ok || strings.HasPrefix(t, ";") {
			continue
		}
		v, ok := set[cur][strings.TrimSpace(k)]
		if !ok {
			continue
		}
		lines[i] = strings.TrimSpace(k) + "=" + v
		if cr {
			lines[i] += "\r"
		}
	}
	return []byte(strings.Join(lines, "\n"))
}
