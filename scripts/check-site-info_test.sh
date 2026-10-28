#!/bin/sh
# Tests for scripts/check-site-info.sh's built-in rules, in a throwaway git
# repository (no commits needed: whole-tree mode scans tracked files, and
# --staged the staged diff). The leaking values are assembled at run time so
# this file itself passes the checker.
#
#   scripts/check-site-info_test.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d) || exit 2
trap 'rm -rf "$work"' EXIT
cd "$work" || exit 2
git init -q .
mkdir scripts
cp "$here/check-site-info.sh" scripts/

fails=0
expect() { # $1 description; $2 wanted exit (0 or 1); $3 text the output must contain; rest: checker args
  desc=$1 want=$2 needle=$3; shift 3
  out=$(scripts/check-site-info.sh "$@" 2>&1); rc=$?
  [ "$rc" -ne 0 ] && rc=1
  if [ "$rc" -ne "$want" ] || ! printf '%s' "$out" | grep -q -F -- "$needle"; then
    echo "FAIL: $desc (exit $rc, want $want; want output containing '$needle')"
    printf '%s\n' "$out" | sed 's/^/    /'
    fails=$((fails + 1))
  else
    echo "ok: $desc"
  fi
}
reset() { git rm -q -r --cached --ignore-unmatch fixture* >/dev/null 2>&1; rm -f fixture*; }

# Clean: documentation addresses, the trailer address, scrubbed comments.
printf 'contact ops@%s or noreply@%s\n' example.net anthropic.com > fixture.md
printf '{"system_cfg": "sshd.auth.key.%s.comment=key-1\\nsshd.auth.key.%s.comment=<comment>\\nsshd.auth.key.%s.comment=\\n"}\n' 1 2 3 > fixture.ndjson
git add fixture.md fixture.ndjson
expect "documentation e-mail and key-N comments pass" 0 "ok: no secrets"
reset

# An e-mail address anywhere else.
dom=corp-mail.org
printf 'owner: someone@%s\n' "$dom" > fixture.md
git add fixture.md
expect "a real-looking e-mail is refused (whole tree)" 1 "someone@$dom"
expect "a real-looking e-mail is refused (--staged)" 1 "someone@$dom" --staged
reset

# A raw SSH key comment, inside an ndjson string and in system_cfg text.
printf '{"system_cfg": "sshd.auth.key.%s.value=<pubkey>\\nsshd.auth.key.%s.comment=%s\\n"}\n' 1 1 builder-laptop > fixture.ndjson
printf 'sshd.auth.key.%s.comment=%s\n' 2 ops-desk > fixture.txt
git add fixture.ndjson fixture.txt
expect "a raw key comment in ndjson is refused, naming file and line" 1 "fixture.ndjson:1: comment=builder-laptop"
expect "a raw key comment in system_cfg text is refused" 1 "fixture.txt:1: comment=ops-desk"
expect "a raw key comment is refused (--staged)" 1 "comment=builder-laptop" --staged
reset

[ "$fails" -eq 0 ] && echo "check-site-info: all rules behave" && exit 0
exit 1
