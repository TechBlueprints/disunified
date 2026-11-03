#!/usr/bin/env bash
# dry-run.sh -- check everything about the IGMP-snooping image build that does
# not need the build host: the series applies to its pinned bases, the result
# is what the work branches hold, the templates the image renders at build and
# boot time render to valid JSON, the YANG models parse, the CLI byte-compiles
# without undefined names, every make variable the new rules use is defined,
# and the daemon's submodule commit can be fetched.
#
# Needs: git, gh is not needed; a python3 with jinja2 and pyang (pyflakes
# optional), e.g.  python3 -m venv /tmp/v && /tmp/v/bin/pip install jinja2 pyang pyflakes
#   PYTHON=/tmp/v/bin/python ./dry-run.sh
# Exit status 0 = every check passed.
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SERIES=$HERE/series
SRC=$HERE/src                      # work clones, used as object stores if present
WORK=${WORK:-$HERE/dryrun}
PY=${PYTHON:-python3}
mkdir -p "$WORK"

declare -A URL=(
  [sonic-buildimage]=https://github.com/sonic-net/sonic-buildimage.git
  [sonic-swss]=https://github.com/sonic-net/sonic-swss.git
  [sonic-swss-common]=https://github.com/sonic-net/sonic-swss-common.git
  [sonic-utilities]=https://github.com/sonic-net/sonic-utilities.git
  [sonic-l2mcd]=https://github.com/jasper-hou-micas/sonic-l2mcd-micas.git
  [sonic-mgmt-common]=https://github.com/sonic-net/sonic-mgmt-common.git
)
declare -A LOCAL=(
  [sonic-buildimage]=sonic-buildimage [sonic-swss]=sonic-swss
  [sonic-swss-common]=sonic-swss-common [sonic-utilities]=sonic-utilities
  [sonic-l2mcd]=sonic-l2mcd-micas [sonic-mgmt-common]=sonic-mgmt-common
)

FAILS=0
pass() { printf '  ok    %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*"; FAILS=$((FAILS + 1)); }
note() { printf '  note  %s\n' "$*"; }

repo_dir() {   # an object store holding the base commit
  local r=$1 base=$2 d
  if [ -d "$SRC/${LOCAL[$r]}/.git" ]; then d=$SRC/${LOCAL[$r]}; else d=$WORK/$r.git; fi
  if [ ! -d "$d" ]; then
    git clone -q --bare --filter=blob:none "${URL[$r]}" "$d" || return 1
  fi
  git -C "$d" cat-file -e "$base^{commit}" 2>/dev/null || git -C "$d" fetch -q origin "$base" || return 1
  echo "$d"
}

# apply <repo> [skip-patch-glob]: index-only git apply of the series on the base;
# prints the resulting tree id
apply_series() {
  local r=$1 skip=${2:-} base d idx p
  base=$(awk -v r="$r" '$1 == r {print $2}' "$SERIES/BASES")
  d=$(repo_dir "$r" "$base") || { echo "cannot fetch $r@$base" >&2; return 1; }
  idx=$(mktemp "$WORK/index.XXXXXX")
  GIT_INDEX_FILE=$idx git -C "$d" read-tree "$base" || return 1
  while read -r p; do
    [ -n "$skip" ] && [[ $p == $skip ]] && continue
    GIT_INDEX_FILE=$idx git -C "$d" apply --cached --whitespace=nowarn "$SERIES/$r/$p" || { echo "$r: $p does not apply" >&2; rm -f "$idx"; return 1; }
  done < "$SERIES/$r/series"
  GIT_INDEX_FILE=$idx git -C "$d" write-tree
  rm -f "$idx"
}

extract() {    # extract <repo> <tree> <dest> <paths...>
  local r=$1 tree=$2 dest=$3; shift 3
  local base d
  base=$(awk -v r="$r" '$1 == r {print $2}' "$SERIES/BASES")
  d=$(repo_dir "$r" "$base")
  rm -rf "$dest"; mkdir -p "$dest"
  git -C "$d" archive "$tree" "$@" | tar -x -C "$dest"
}

echo "== 1. the series applies to its bases (git apply --cached, no fuzz)"
declare -A TREE
for r in sonic-buildimage sonic-swss sonic-swss-common sonic-utilities sonic-l2mcd sonic-mgmt-common; do
  if t=$(apply_series "$r"); then
    TREE[$r]=$t; pass "$r: $(wc -l < "$SERIES/$r/series" | tr -d ' ') patch(es) -> tree ${t:0:12}"
    if [ -d "$SRC/${LOCAL[$r]}/.git" ]; then
      w=$(git -C "$SRC/${LOCAL[$r]}" rev-parse 'igmp-202511^{tree}' 2>/dev/null || true)
      if [ -n "$w" ]; then
        [ "$w" = "$t" ] && pass "$r: equals the igmp-202511 work branch" || fail "$r: differs from the work branch tree ${w:0:12}"
      fi
    fi
  else
    fail "$r: series does not apply"
  fi
done
if t=$(apply_series sonic-buildimage '0005-*'); then
  pass "sonic-buildimage without 0005 (DX010_SAI=xgs) -> tree ${t:0:12}"
else
  fail "sonic-buildimage without 0005 does not apply"
fi
[ "$FAILS" -eq 0 ] || { echo "stopping: $FAILS failure(s)"; exit 1; }

echo "== 2. extract what the remaining checks read"
BI=$WORK/tree/sonic-buildimage
extract sonic-buildimage "${TREE[sonic-buildimage]}" "$BI" \
  .gitmodules rules slave.mk dockers/dockerfile-macros.j2 dockers/docker-l2mcd \
  files/build_templates/init_cfg.json.j2 files/build_templates/per_namespace/l2mcd.service.j2 \
  files/image_config/copp/copp_cfg.j2 src/sonic-yang-models/setup.py \
  src/sonic-yang-models/yang-models src/sonic-yang-models/yang-templates \
  src/sonic-yang-models/tests/files/sample_config_db.json \
  device/celestica/x86_64-cel_seastone-r0 platform/broadcom
SW=$WORK/tree/sonic-swss
extract sonic-swss "${TREE[sonic-swss]}" "$SW" orchagent/copporch.cpp orchagent/saihelper.cpp cfgmgr/Makefile.am cfgmgr/l2mcmgr.h
UT=$WORK/tree/sonic-utilities
extract sonic-utilities "${TREE[sonic-utilities]}" "$UT" config show debug scripts/fast-reboot
UTB=$WORK/tree/sonic-utilities-base
extract sonic-utilities "$(awk '$1 == "sonic-utilities" {print $2}' "$SERIES/BASES")" "$UTB" config show debug
LM=$WORK/tree/sonic-l2mcd
extract sonic-l2mcd "${TREE[sonic-l2mcd]}" "$LM" .
pass "extracted under $WORK/tree"

echo "== 3. the daemon submodule"
gl=$(git -C "$(repo_dir sonic-buildimage "$(awk '$1=="sonic-buildimage"{print $2}' "$SERIES/BASES")")" ls-tree "${TREE[sonic-buildimage]}" src/sonic-l2mcd | awk '{print $1, $3}')
lbase=$(awk '$1 == "sonic-l2mcd" {print $2}' "$SERIES/BASES")
[ "$gl" = "160000 $lbase" ] && pass "gitlink src/sonic-l2mcd = $lbase" || fail "gitlink is '$gl', expected 160000 $lbase"
url=$(git config -f "$BI/.gitmodules" submodule.src/sonic-l2mcd.url)
[ "$url" = "https://github.com/jasper-hou-micas/sonic-l2mcd-micas" ] && pass ".gitmodules url $url" || fail ".gitmodules url $url"
if git ls-remote --exit-code "$url" refs/heads/dev >/dev/null 2>&1; then pass "$url answers"; else fail "$url does not answer"; fi
[ -f "$LM/debian/control" ] && grep -q '^Package: l2mcd' "$LM/debian/control" && pass "daemon has debian/ packaging for l2mcd" || fail "no debian/control in the daemon"
v=$(sed -n '1s/.*(\(.*\)).*/\1/p' "$LM/debian/changelog")
grep -q "L2MCD_VERSION = $v" "$BI/rules/l2mcd.mk" && pass "rules/l2mcd.mk version $v matches debian/changelog" || fail "l2mcd version $v not what rules/l2mcd.mk builds"
[ -f "$LM/include/l2mcd_ipc.h" ] && grep -q 'top_srcdir)/../sonic-l2mcd/include' "$SW/cfgmgr/Makefile.am" && pass "swss finds l2mcd_ipc.h at src/sonic-l2mcd/include" || fail "l2mcd_ipc.h path"
if grep -n 'memcpy([^;]*c_str()[^;]*\(IFNAME_SIZE\|ADDR_STR_SIZE\))' "$LM/l2mcd_sync.cpp"; then fail "fixed-size memcpy from c_str() left in l2mcd_sync.cpp"; else pass "no fixed-size memcpy from c_str() in l2mcd_sync.cpp"; fi

echo "== 4. templates render"
"$PY" -I - "$BI" "$SW" <<'PY' || FAILS=$((FAILS + 1))
import json, re, sys, os
import jinja2
bi, sw = sys.argv[1], sys.argv[2]
ok = lambda m: print("  ok    " + m)
bad = []
def render(path, **v):
    env = jinja2.Environment(loader=jinja2.FileSystemLoader(bi), extensions=["jinja2.ext.do"],
                             undefined=jinja2.ChainableUndefined, keep_trailing_newline=True)
    return env.get_template(path).render(**v)
# CoPP: valid JSON, the trap group, every trap id known to copporch
copp = json.loads(render("files/image_config/copp/copp_cfg.j2"))
g = copp["COPP_GROUP"].get("l2mcd_group"); t = copp["COPP_TRAP"].get("l2mcd")
if not g or not t or t.get("trap_group") != "l2mcd_group":
    bad.append("copp_cfg: l2mcd trap/group missing")
else:
    known = set(re.findall(r'\{\s*"([a-z0-9_]+)"\s*,\s*SAI_HOSTIF_TRAP_TYPE_', open(os.path.join(sw, "orchagent/copporch.cpp")).read()))
    missing = [x for x in t["trap_ids"].split(",") if x not in known]
    if missing: bad.append("copp trap ids unknown to copporch: %s" % missing)
    else: ok("copp_cfg.j2 renders; l2mcd group %s pps queue %s; %d trap ids all in copporch" % (g["cir"], g["queue"], len(t["trap_ids"].split(","))))
    if t.get("always_enabled") == "true": bad.append("l2mcd trap is always_enabled")
    # the ECS's pattern: IGMP only, 128/160 pps, queue 1 (series 0006)
    if any(x.startswith("mld") for x in t["trap_ids"].split(",")): bad.append("copp traps MLD")
    elif (g["cir"], g["cbs"], g["queue"]) != ("128", "160", "1"): bad.append("l2mcd_group is not 128/160 pps on queue 1")
    else: ok("copp follows the ECS: IGMP only, no MLD, 128/160 pps, queue 1")
# init_cfg: valid JSON, FEATURE l2mcd disabled, no MGMT_PORT default
v = dict(include_l2mcd="y", sonic_asic_platform="broadcom", installer_services="", DEVICE_METADATA={"localhost": {}})
init = json.loads(render("files/build_templates/init_cfg.json.j2", **v))
f = init.get("FEATURE", {}).get("l2mcd")
if not f: bad.append("init_cfg: no FEATURE|l2mcd")
elif f.get("state") != "disabled": bad.append("init_cfg: l2mcd state %r" % f.get("state"))
else: ok("init_cfg.json.j2 renders; FEATURE|l2mcd state disabled, auto_restart %s" % f.get("auto_restart"))
if "MGMT_PORT" in init: bad.append("init_cfg still sets MGMT_PORT")
else: ok("init_cfg.json.j2 sets no MGMT_PORT default")
init_n = json.loads(render("files/build_templates/init_cfg.json.j2", **dict(v, include_l2mcd="n")))
("l2mcd" in init_n.get("FEATURE", {})) and bad.append("init_cfg: l2mcd present with INCLUDE_L2MCD=n")
# Dockerfile and service
df = render("dockers/docker-l2mcd/Dockerfile.j2", docker_l2mcd_debs="l2mcd_1.0.0_amd64.deb swss_1.0.0_amd64.deb", DOCKER_USERNAME="u", DOCKER_USERTAG="t")
"docker-config-engine-bookworm-u:t" in df or bad.append("Dockerfile base is not docker-config-engine-bookworm")
"debs/l2mcd_1.0.0_amd64.deb" in df or bad.append("Dockerfile does not install the l2mcd deb")
ok("docker-l2mcd Dockerfile.j2 renders (bookworm base, installs the debs)")
svc = render("files/build_templates/per_namespace/l2mcd.service.j2", docker_container_name="l2mcd", multi_instance="false", sonicadmin_user="admin")
("ExecStartPre=/usr/bin/l2mcd.sh start" in svc and "Requires=swss.service" in svc) or bad.append("l2mcd.service renders wrong")
ok("l2mcd.service.j2 renders (after swss, l2mcd.sh start/wait/stop)")
json.load(open(os.path.join(bi, "src/sonic-yang-models/tests/files/sample_config_db.json")))
ok("sample_config_db.json is valid JSON")
for m in bad: print("  FAIL  " + m)
sys.exit(1 if bad else 0)
PY

echo "== 5. YANG"
YM=$WORK/tree/yang
rm -rf "$YM"; mkdir -p "$YM"; cp "$BI"/src/sonic-yang-models/yang-models/*.yang "$YM"/
"$PY" -I - "$BI/src/sonic-yang-models/yang-templates" "$YM" <<'PY2' || FAILS=$((FAILS + 1))
import glob, os, sys, jinja2
src, dst = sys.argv[1], sys.argv[2]
env = jinja2.Environment(loader=jinja2.FileSystemLoader(src), trim_blocks=True)
for f in glob.glob(src + "/*.yang.j2"):
    b = os.path.basename(f)
    open(os.path.join(dst, b[:-3]), "w").write(env.get_template(b).render(yang_model_type="py"))
PY2
for y in sonic-igmp-snooping sonic-mld-snooping; do
  if out=$("$PY" -m pyang -p "$YM" "$YM/$y.yang" 2>&1); then pass "pyang $y.yang"; else fail "pyang $y.yang"; echo "$out" | head -20; fi
  grep -q "'$y.yang'" "$BI/src/sonic-yang-models/setup.py" && pass "$y.yang listed in setup.py" || fail "$y.yang not in setup.py"
done

echo "== 6. rules"
"$PY" -I - "$BI" <<'PY' || FAILS=$((FAILS + 1))
import re, sys, glob, os
bi = sys.argv[1]
defined = set()
for f in glob.glob(bi + "/rules/*.mk") + glob.glob(bi + "/rules/*.dep") + glob.glob(bi + "/platform/broadcom/*.mk") + [bi + "/slave.mk", bi + "/rules/config"]:
    for m in re.finditer(r'^\s*(?:export\s+)?([A-Z][A-Z0-9_]*)\s*[:+?]?=', open(f).read(), re.M):
        defined.add(m.group(1))
defined |= {"DBG_IMAGE_MARK", "CONFIGURED_ARCH", "SRC_PATH", "DOCKERS_PATH", "PLATFORM_PATH", "TARGET_PATH",
            "SONIC_COMMON_FILES_LIST", "SONIC_COMMON_BASE_FILES_LIST", "SONIC_COMMON_FLAGS_LIST"}
used = {}
for f in ["rules/l2mcd.mk", "rules/l2mcd.dep", "rules/docker-l2mcd.mk", "rules/docker-l2mcd.dep"]:
    text = "\n".join(l for l in open(os.path.join(bi, f)).read().splitlines() if not l.lstrip().startswith("#"))
    for v in re.findall(r'\$\(([A-Z][A-Z0-9_]*)\)', text):
        used.setdefault(v, f)
undef = sorted(v for v in used if v not in defined)
expected = {"LIBEVENT"}   # never defined in 202511: expands empty; libevent-dev is in the bookworm slave
for v in undef:
    print(("  note  " if v in expected else "  FAIL  ") + "%s used in %s is not defined (expands empty)" % (v, used[v]))
ok = not [v for v in undef if v not in expected]
print("  ok    %d make variables used by the l2mcd rules, all defined%s" % (len(used), " but LIBEVENT" if "LIBEVENT" in undef else "") if ok else "")
sys.exit(0 if ok else 1)
PY
grep -q 'INCLUDE_L2MCD ?= y' "$BI/rules/config" && pass "rules/config: INCLUDE_L2MCD ?= y" || fail "INCLUDE_L2MCD"
grep -q '^# ENABLE_TRANSLIB_WRITE = y' "$BI/rules/config" && pass "rules/config: translib write left at the 202511 default" || fail "ENABLE_TRANSLIB_WRITE changed"
grep -q 'SMDEP_SPATH   := $(SPATH)$' "$BI/rules/l2mcd.dep" && pass "rules/l2mcd.dep: SMDEP path is the submodule" || fail "rules/l2mcd.dep SMDEP path"
if grep -q '_DEPENDS .*$(LIBYANG3_DEV) $(LIBYANG3)' "$BI/rules/sonic-mgmt-framework.mk" && grep -q '$(LIBYANG3_DEV) $(LIBYANG3)' "$BI/rules/telemetry.mk" \
   && grep -q 'SONIC_MGMT_COMMON)_DEPENDS  = $(LIBYANG3_DEV) $(LIBYANG3)' "$BI/rules/sonic-mgmt-common.mk" \
   && ! grep -rq '$(LIBYANG_DEV)' "$BI/rules/sonic-mgmt-common.mk" "$BI/rules/sonic-mgmt-framework.mk" "$BI/rules/telemetry.mk" "$BI/rules/swss-common.mk"; then
  pass "swss-common, mgmt-common, mgmt-framework and gnmi build against libyang-dev 3 (0008)"
else fail "0008 libyang-dev 3"; fi
[ ! -e "$BI/rules/libyang3-dev-done.mk" ] && ! grep -q 'LIBYANG3-DEV-DONE' "$BI/rules/libyang.mk" && pass "libyang3-dev-done retired (0009)" || fail "0009"
grep -q '^INCLUDE_SYSTEM_GNMI = y$' "$BI/rules/config" && grep -q '^INCLUDE_MGMT_FRAMEWORK = y$' "$BI/rules/config" && pass "gNMI and mgmt-framework built (the image keeps them)" || fail "gNMI/mgmt-framework left out"
bash -n "$BI/dockers/docker-l2mcd/start.sh" && pass "docker-l2mcd start.sh parses" || fail "start.sh"

echo "== 7. DX010 platform patches"
D=$BI/device/celestica/x86_64-cel_seastone-r0
n=$(grep -l '^SAI_STATS_ST_CAPABILITY_SUPPORTED=0$' "$D"/*/sai.profile* | wc -l | tr -d ' ')
m=$(grep -l '^SAI_STATS_EXT_SWITCH_SUPPORTED=0$' "$D"/*/sai.profile* | wc -l | tr -d ' ')
[ "$n" = 8 ] && [ "$m" = 8 ] && pass "all 8 Seastone sai.profiles carry both guards" || fail "sai.profile guards: $n/$m of 8"
[ "$(cat "$D/platform_asic")" = broadcom-legacy-th ] && pass "platform_asic = broadcom-legacy-th (image: sonic-broadcom-legacy-th.bin)" || fail "platform_asic"
grep -q 'LAZY_BUILD_INSTALLS = .*BRCM_LEGACY_TH_OPENNSL_KERNEL' "$BI/platform/broadcom/one-image.mk" && pass "one-image.mk installs the legacy-TH BDE modules" || fail "one-image.mk"
CEL=$BI/platform/broadcom/sonic-platform-modules-cel/debian
grep -q '^build-dx010/sonic_platform-1.0-py3-none-any.whl usr/share/sonic/device/x86_64-cel_seastone-r0$' "$CEL/platform-modules-dx010.install" && grep -q 'python3 setup.py bdist_wheel -d $(MOD_SRC_DIR)/build-$${mod})' "$CEL/rules" && pass "the DX010's sonic_platform wheel is built and installed for pmon (0007, sonic-net/sonic-buildimage#27355)" || fail "0007: DX010 wheel"
grep -q '^LIBSAIBCM_LEGACY_TH_VERSION = ' "$BI/platform/broadcom/sai-legacy-th.mk" && note "legacy-TH SAI $(sed -n 's/^LIBSAIBCM_LEGACY_TH_VERSION = //p' "$BI/platform/broadcom/sai-legacy-th.mk"), XGS SAI $(sed -n 's/^LIBSAIBCM_XGS_VERSION = //p' "$BI/platform/broadcom/sai-xgs.mk")"

echo "== 8. CLI"
for f in config/main.py config/vlan.py show/main.py debug/main.py; do
  "$PY" -m py_compile "$UT/$f" 2>/dev/null && pass "py_compile $f" || fail "py_compile $f"
done
if "$PY" -c 'import pyflakes' 2>/dev/null; then
  undef() { (cd "$1" && "$PY" -m pyflakes config/main.py config/vlan.py show/main.py debug/main.py 2>&1 | grep 'undefined name' | sed -E 's/:[0-9]+:[0-9]+:/:/' | sort); }
  new=$(comm -13 <(undef "$UTB") <(undef "$UT"))
  old=$(undef "$UTB" | wc -l | tr -d ' ')
  [ -z "$new" ] && pass "pyflakes: no undefined names added ($old already in 202511)" || { fail "pyflakes: undefined names added"; echo "$new"; }
else
  note "pyflakes not installed; undefined-name check skipped"
fi
bash -n "$UT/scripts/fast-reboot" && pass "scripts/fast-reboot parses" || fail "fast-reboot"

echo
if [ "$FAILS" -eq 0 ]; then echo "dry run: all checks passed"; else echo "dry run: $FAILS check(s) failed"; fi
exit $(( FAILS > 0 ))
