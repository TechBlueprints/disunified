package aristaeos

import "bytes"

// finiteJSON rewrites the number tokens EOS prints that JSON does not have
// -- -Infinity (a power of zero in dBm: an optic with no light), Infinity and
// NaN -- as null, wherever they stand outside a string. One of them anywhere
// in a reply fails the whole reply otherwise, and with it every command of
// the poll (4.26.14M, `show interfaces transceiver dom thresholds`, the
// `aggrRxPwr` of a module with no receive signal:
// docs/fixtures/arista-eos-4.26.14M/dom-no-light/).
func finiteJSON(raw []byte) []byte {
	if !bytes.Contains(raw, []byte("Infinity")) && !bytes.Contains(raw, []byte("NaN")) {
		return raw
	}
	out := make([]byte, 0, len(raw))
	inString, escaped := false, false
	for i := 0; i < len(raw); {
		c := raw[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			i++
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			i++
			continue
		}
		if n := nonFiniteToken(raw[i:]); n > 0 {
			out = append(out, "null"...)
			i += n
			continue
		}
		out = append(out, c)
		i++
	}
	return out
}

// nonFiniteToken is the length of a non-finite number token at the start of
// b, or 0.
func nonFiniteToken(b []byte) int {
	for _, t := range []string{"-Infinity", "Infinity", "NaN"} {
		if bytes.HasPrefix(b, []byte(t)) {
			return len(t)
		}
	}
	return 0
}
