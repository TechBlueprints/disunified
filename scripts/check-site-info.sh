#!/bin/sh
# Fails if tracked files (or the staged diff, with --staged) contain anything
# that looks like a secret or a site identifier. Run before every commit.
#
#   scripts/check-site-info.sh            # scan every tracked file
#   scripts/check-site-info.sh --staged   # scan only what is about to be committed
#
# Allowed vocabulary: documentation addresses (192.0.2/24, 198.51.100/24,
# 203.0.113/24, 2001:db8::/32), MACs 02:00:00:xx:xx:xx / aa:bb:cc:dd:ee:ff,
# serial SSJ00000000, authkeys 0123456789abcdef0123456789abcdef and
# 000...0NN, example.net names, e-mail only at example.com/.net/.org (and
# the commit trailer's noreply@anthropic.com), SSH key comments only as
# key-N (or <comment>/empty). Site-specific words to deny (your domain,
# device names, hostnames) go one per line in local-information/denylist.txt
# (gitignored); the script reads it when present.
set -u
cd "$(git rev-parse --show-toplevel)" || exit 2
tmp=$(mktemp) || exit 2
trap 'rm -f "$tmp"' EXIT
if [ "${1:-}" = "--staged" ]; then
  git diff --cached -U0 | grep -E '^\+' | grep -v -E '^\+\+\+' > "$tmp"
else
  git grep -n -I -E '.' -- . ':!go.sum' > "$tmp"
fi
fail=0
check() { # $1 label; $2 matches (possibly empty)
  if [ -n "$2" ]; then
    echo "!! $1"; printf '%s\n' "$2" | head -n 10 | sed 's/^/   /'; fail=1
  fi
}
check "key material" \
  "$(grep -E 'BEGIN [A-Z ]*PRIVATE KEY|ssh-(rsa|ed25519|dss) AAAA|ecdsa-sha2-nistp[0-9]+ AAAA' "$tmp" | cut -c1-120)"
check "authkey-shaped values that are not a documented dummy" \
  "$(grep -o -i -E '(^|[^0-9a-f])[0-9a-f]{32}([^0-9a-f]|$)' "$tmp" | tr -d -c '0-9a-fA-F\n' | grep -v -i -E '^(0123456789abcdef0123456789abcdef|0{30}[0-9a-f]{2}|ba86f2bbe107c7c57eb5f2690775c712)$' | sort -u)"
check "private (RFC 1918) addresses; use 192.0.2.0/24, 198.51.100.0/24 or 203.0.113.0/24" \
  "$(grep -o -E '(^|[^0-9.])(10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|192\.168\.[0-9]{1,3}\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3})([^0-9.]|$)' "$tmp" | tr -d -c '0-9.\n' | grep -v -E '^10\.6\.[0-9]+$' | sort -u)"
check "MAC addresses outside the scrub vocabulary (02:00:00:xx:xx:xx, aa:bb:cc:dd:ee:ff)" \
  "$(grep -o -i -E '(^|[^0-9a-f:.])(([0-9a-f]{2}[:.]){5}[0-9a-f]{2}|[0-9a-f]{4}\.[0-9a-f]{4}\.[0-9a-f]{4})([^0-9a-f:.]|$)' "$tmp" | sed -E 's/^[^0-9a-fA-F]//; s/[^0-9a-fA-F]$//' | grep -v -i -E '^02:00:00:|^0200\.0000\.|^00:00:00:00:00:0|^aa:bb:cc:dd:ee:ff$|^aabb\.ccdd\.eeff$|^ff:ff:ff:ff:ff:ff$|^01:80:c2|^01:00:5e|^33:33:|^02:53:54:55' | sort -u)"
check "serial numbers (use SSJ00000000)" \
  "$(grep -o -E '(^|[^A-Z0-9])[A-Z]{3}[0-9]{8}([^A-Z0-9]|$)' "$tmp" | tr -d -c 'A-Z0-9\n' | grep -v '^SSJ00000000$' | sort -u)"
# E-mail addresses are personal data (an SSH key comment carried an employer
# address into a fixture once): only the documentation domains and the
# commit trailer's address are allowed.
check "e-mail addresses (use name@example.net)" \
  "$(grep -o -E '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' "$tmp" | grep -v -i -E '@([A-Za-z0-9-]+\.)*example\.(com|net|org)$|^noreply@anthropic\.com$' | sort -u)"
# SSH key comments in controller captures (sshd.auth.key.N.comment=, in
# system_cfg text or inside ndjson strings) name the key's owner or machine:
# scripts/sanitize-controller.py makes them key-N; nothing else is allowed.
check "SSH key comments that are not key-N (scripts/sanitize-controller.py --comments-only <file>)" \
  "$(awk -v staged="${1:-}" '{
      s = $0
      while (match(s, /sshd\.auth\.key\.[0-9]+\.comment=[^\\"]*/)) {
        v = substr(s, RSTART, RLENGTH); sub(/^sshd\.auth\.key\.[0-9]+\.comment=/, "", v)
        if (v != "" && v != "<comment>" && v !~ /^key-[0-9]+$/) {
          if (staged == "--staged") { where = "staged: " substr($0, 1, 60) } else { split($0, p, ":"); where = p[1] ":" p[2] }
          print where ": comment=" v
        }
        s = substr(s, RSTART + RLENGTH)
      }
    }' "$tmp" | sort -u)"
if [ -s local-information/denylist.txt ]; then
  check "site words from local-information/denylist.txt" \
    "$(grep -i -F -f local-information/denylist.txt "$tmp" | cut -c1-120)"
fi
[ "$fail" -eq 0 ] && echo "ok: no secrets or site identifiers found"
exit $fail
