package sonic

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Spanning tree and loop protection where the image has no STP daemon
// (docs/drivers/sonic.md §11.5): disunified-l2d (l2d.py) on the switch runs
// mstpd on the kernel bridge and mirrors its port states into the stock
// orchagent's StpOrch; this file installs it, writes its config from the
// controller's push and reads its view back. An image with FEATURE|stp
// (community stpd, PVST) keeps the `config spanning-tree` path instead.

var (
	//go:embed l2d.py
	l2dScript string
	//go:embed bridge-stp.sh
	bridgeSTPScript string
	//go:embed disunified-l2d.service
	l2dUnit string
	//go:embed build-mstpd.sh
	buildMstpdScript string
)

// Where the pieces live on the switch. /etc/sonic survives an image
// upgrade (the config); the rest lives in the image's overlay and is
// installed again after one.
const (
	l2dPath      = "/usr/local/sbin/disunified-l2d"
	bridgeSTP    = "/usr/sbin/bridge-stp" // the kernel calls /sbin/bridge-stp; /sbin is usr/sbin on Debian 12
	l2dUnitPath  = "/etc/systemd/system/disunified-l2d.service"
	l2ConfPath   = "/etc/sonic/disunified/l2.json"
	mstpdBuildIn = "/var/lib/disunified/mstpd-build"

	l2Command         = "bash -s -- disunified-l2"
	mstpdBuildCommand = "bash -s -- disunified-mstpd-build"
	// l2DeadmanCancel runs over a fresh connection once STP is up and the
	// switch still answers.
	l2DeadmanCancel = "sudo systemctl stop disunified-l2d-deadman.timer"
	l2CheckCommand  = "sonic-db-cli STATE_DB HGET 'DUI_STP_TABLE|Bridge' mstpd"
	// l2DeadmanAfter is how long a first enable may leave the switch
	// unreachable before the switch undoes it by itself.
	l2DeadmanAfter = 180 * time.Second
)

// Loop protection as Ubiquiti's own SONiC switches run it
// (MAC_MOVE_GUARD|GLOBAL on the ECS Aggregation, §9).
const (
	loopThreshold = 10  // MAC moves
	loopInterval  = 5   // within this many seconds
	loopHold      = 150 // seconds the port stays down
)

// l2Conf is l2d's config file (l2d.py load_conf).
type l2Conf struct {
	STP       l2STP   `json:"stp"`
	LoopGuard l2Guard `json:"loop_guard"`
	// Deadman is set by the switch when the dead-man timer undid a first
	// enable: the time it fired. The driver does not enable STP again until
	// the controller has turned it off and on.
	Deadman string `json:"deadman,omitempty"`
}

type l2STP struct {
	Enabled  bool              `json:"enabled"`
	Version  string            `json:"version"`
	Priority int               `json:"priority"`
	Ports    map[string]l2Port `json:"ports,omitempty"`
}

type l2Port struct {
	Enabled   bool   `json:"enabled"`
	Edge      string `json:"edge"`
	BPDUGuard bool   `json:"bpdu_guard"`
}

type l2Guard struct {
	Enabled   bool     `json:"enabled"`
	Threshold int      `json:"threshold"`
	Interval  int      `json:"interval"`
	Hold      int      `json:"hold"`
	Ports     []string `json:"ports,omitempty"`
}

// l2Inst is collect.py's l2d section: what is installed and running.
type l2Inst struct {
	Files    map[string]string `json:"files"` // path -> sha256, "" = absent
	Mstpd    string            `json:"mstpd"` // `mstpd -V`, "" = not installed
	Service  string            `json:"service"`
	Deadman  string            `json:"deadman"`
	Build    string            `json:"build"` // "running", "ok", "failed: …", ""
	Docker   bool              `json:"docker"`
	STPState string            `json:"stp_state"`
	Conf     *l2Conf           `json:"conf"`
	present  bool              // the section was in the capture
}

func parseL2Inst(body string) l2Inst {
	var in l2Inst
	body = strings.TrimSpace(body)
	if body == "" || body == "error" {
		return in
	}
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		return l2Inst{}
	}
	in.present = true
	return in
}

// canRun: the switch can run disunified-l2d -- mstpd is there, or it can
// be built there.
func (in l2Inst) canRun() bool { return in.present && (in.Mstpd != "" || in.Docker) }

// l2Want is the controller's intent, assembled from ApplyDevice (the bridge)
// and ApplyPorts (the ports); written only once both have been seen.
type l2Want struct {
	stpSet   bool
	stp      bool
	version  string
	priority int
	portsSet bool
	ports    map[string]l2Port
	guard    []string
	// stpOffSeen: the push had STP off since the dead-man last fired,
	// which re-arms a first enable.
	stpOffSeen  bool
	buildTried  time.Time
	deadmanSeen string
}

// anyPortSetting: a port asks for BPDU guard, STP off, or loop protection.
func (w *l2Want) anyPortSetting() bool {
	if len(w.guard) > 0 {
		return true
	}
	for _, p := range w.ports {
		if p.BPDUGuard || !p.Enabled {
			return true
		}
	}
	return false
}

func (w *l2Want) conf() l2Conf {
	c := l2Conf{
		STP:       l2STP{Enabled: w.stp, Version: w.version, Priority: w.priority, Ports: w.ports},
		LoopGuard: l2Guard{Enabled: len(w.guard) > 0, Threshold: loopThreshold, Interval: loopInterval, Hold: loopHold, Ports: w.guard},
	}
	if c.STP.Version == "" {
		c.STP.Version = "rstp"
	}
	if c.STP.Priority == 0 {
		c.STP.Priority = 32768
	}
	return c
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// l2SetSTP records the bridge-level intent (ApplyDevice).
func (c *Collector) l2SetSTP(d devicemodel.DeviceDesired) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !d.STPSet {
		return
	}
	c.l2.stpSet = true
	c.l2.stp = d.STPEnabled
	c.l2.version = strings.ToLower(d.STPMode)
	c.l2.priority = d.STPPriority
	if !d.STPEnabled {
		c.l2.stpOffSeen = true
	}
}

// l2SetPorts records the per-port intent (ApplyPorts): every interface of
// a cage, or the PortChannel a cage is a member of (the kernel bridge port).
func (c *Collector) l2SetPorts(desired []devicemodel.PortDesired, st *swState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ports := map[string]l2Port{}
	var guard []string
	seen := map[string]bool{}
	for _, d := range desired {
		ifs := c.ifnames[d.Index]
		if len(ifs) == 0 {
			continue
		}
		edge := d.STPEdgeMode
		if edge == "" {
			edge = "auto"
		}
		pc := l2Port{Enabled: !d.STPEdge, Edge: edge, BPDUGuard: d.BPDUGuard}
		names := ifs
		if lag := st.lagOf(ifs[0]); lag != "" {
			names = []string{lag}
		}
		for _, n := range names {
			if seen[n] {
				continue // a PortChannel takes its lowest member's settings
			}
			seen[n] = true
			ports[n] = pc
			if d.LoopProtect {
				guard = append(guard, n)
			}
		}
	}
	sort.Strings(guard)
	c.l2.portsSet, c.l2.ports, c.l2.guard = true, ports, guard
}

// applyL2 brings disunified-l2d's installation and config in line with the
// intent. It returns the number of writes it made.
func (c *Collector) applyL2(ctx context.Context) (int, error) {
	c.mu.Lock()
	st := c.state
	w := c.l2
	c.mu.Unlock()
	if st == nil || st.stpFeature {
		return 0, nil
	}
	in := st.l2
	if !c.L2 || !in.canRun() {
		if (w.stpSet && w.stp) || w.anyPortSetting() {
			why := "the collector reported no disunified-l2d state (an older collect.py)"
			switch {
			case !c.L2:
				why = "disunified-l2d is off for this device (options.l2d: off)"
			case in.present:
				why = "the switch has no mstpd and no docker to build it"
			}
			c.warnOnce("l2 cannot", "sonic: spanning tree, BPDU guard or loop protection requested, but this image has no STP daemon (no FEATURE|stp) and %s; not written", why)
		}
		return 0, nil
	}
	if !w.stpSet || !w.portsSet {
		return 0, nil
	}
	want := w.conf()
	need := want.STP.Enabled || want.LoopGuard.Enabled
	if !need && in.Conf == nil {
		return 0, nil // nothing wanted, nothing installed
	}
	if in.Conf != nil && in.Conf.Deadman != "" {
		if in.Conf.Deadman != w.deadmanSeen {
			c.mu.Lock()
			c.l2.deadmanSeen, c.l2.stpOffSeen = in.Conf.Deadman, false
			c.mu.Unlock()
			w.stpOffSeen = false
		}
		if want.STP.Enabled && !w.stpOffSeen {
			c.warnOnce("l2 deadman "+in.Conf.Deadman, "sonic: the switch's dead-man timer undid spanning tree at %s (the switch did not answer a fresh connection after it came up); not enabled again until STP is turned off and on in UniFi (docs/drivers/sonic.md §11.5)", in.Conf.Deadman)
			want.STP.Enabled = false
			want.Deadman = in.Conf.Deadman
		}
	}
	started := 0
	if want.STP.Enabled && in.Mstpd == "" {
		want.STP.Enabled = false
		switch {
		case in.Build == "running":
			c.warnOnce("l2 building", "sonic: mstpd is being built on the switch; spanning tree comes up once it is installed")
		case !in.Docker:
			c.warnOnce("l2 nodocker", "sonic: spanning tree requested, but the switch has no mstpd and no docker to build it; not enabled")
		case time.Since(w.buildTried) < 10*time.Minute:
			if strings.HasPrefix(in.Build, "failed") {
				c.warnOnce("l2 build "+in.Build, "sonic: building mstpd on the switch %s; retried every 10 minutes", in.Build)
			}
		default:
			c.mu.Lock()
			c.l2.buildTried = time.Now()
			c.mu.Unlock()
			c.Log.Printf("sonic apply l2: building mstpd %s on the switch (static, from the upstream source, in a throwaway alpine container; about a minute)", mstpdVersion)
			if _, err := c.run(ctx, mstpdBuildCommand, mstpdBuildHostScript()); err != nil {
				return 0, fmt.Errorf("sonic apply l2: start the mstpd build: %w", err)
			}
			started = 1
		}
		if !want.LoopGuard.Enabled && in.Conf == nil {
			return started, nil // installed once mstpd is there
		}
	}

	var steps []string
	files := []struct{ path, mode, body string }{
		{l2dPath, "0755", l2dScript},
		{bridgeSTP, "0755", bridgeSTPScript},
		{l2dUnitPath, "0644", l2dUnit},
	}
	unitChanged := false
	for _, f := range files {
		if in.Files[f.path] == sha(f.body) {
			continue
		}
		steps = append(steps, installFile(f.path, f.mode, f.body))
		unitChanged = unitChanged || f.path == l2dUnitPath
	}
	if unitChanged {
		steps = append(steps, "sudo systemctl daemon-reload", "sudo systemctl enable disunified-l2d >/dev/null 2>&1")
	}
	confChanged := in.Conf == nil || !reflect.DeepEqual(*in.Conf, want)
	if confChanged {
		b, _ := json.MarshalIndent(want, "", "  ")
		steps = append(steps, installFile(l2ConfPath, "0644", string(b)+"\n"))
	}
	// A first enable runs under a dead-man timer that tears STP down again
	// unless a fresh connection cancels it (§11.3).
	running := in.Service == "active"
	arm := want.STP.Enabled && !(running && in.Conf != nil && in.Conf.STP.Enabled)
	if arm {
		steps = append(steps,
			"sudo systemctl stop disunified-l2d-deadman.timer >/dev/null 2>&1 || true",
			fmt.Sprintf("sudo systemd-run --quiet --unit=disunified-l2d-deadman --on-active=%d /usr/bin/python3 %s --deadman", int(l2DeadmanAfter.Seconds()), l2dPath))
	}
	switch {
	case !running || unitChanged || in.Files[l2dPath] != sha(l2dScript):
		steps = append(steps, "sudo systemctl restart disunified-l2d")
	case confChanged:
		steps = append(steps, "sudo systemctl reload disunified-l2d")
	}
	if len(steps) == 0 {
		return started, nil
	}
	c.Log.Printf("sonic apply l2: %s", l2Summary(in, want, confChanged, arm))
	if _, err := c.run(ctx, l2Command, "set -e\n"+strings.Join(steps, "\n")+"\n"); err != nil {
		return 0, fmt.Errorf("sonic apply l2: %w", err)
	}
	c.mu.Lock()
	if c.state == st {
		next := st.clone()
		next.l2.Conf = &want
		next.l2.Service = "active"
		for _, f := range files {
			next.l2.Files[f.path] = sha(f.body)
		}
		if arm {
			next.l2.Deadman = "active"
		}
		c.state = next
	}
	c.mu.Unlock()
	if arm {
		if err := c.confirmL2(ctx); err != nil {
			return 1, fmt.Errorf("sonic apply l2: %w (the switch undoes spanning tree by itself after %s)", err, l2DeadmanAfter)
		}
	}
	return 1, nil
}

// confirmL2 dials the switch afresh once spanning tree is up and, if it
// answers and mstpd runs, cancels the dead-man timer through that
// connection. A first enable starts every port discarding: the root port
// forwards again as soon as the root's next BPDU arrives (hello 2 s).
func (c *Collector) confirmL2(ctx context.Context) error {
	if c.Redial == nil {
		return fmt.Errorf("no way to dial the switch afresh; the dead-man timer stays")
	}
	var last error
	for i := 0; i < 10; i++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("spanning tree not confirmed: %v", ctx.Err())
		case <-time.After(c.l2Wait()):
		}
		r := c.Redial(c.Dial)
		out, err := r.Run(ctx, l2CheckCommand, "")
		if err == nil && strings.TrimSpace(out) == "running" {
			_, err = r.Run(ctx, l2DeadmanCancel, "")
			r.Close()
			if err == nil {
				c.Log.Printf("sonic apply l2: spanning tree up, the switch answers a fresh connection; dead-man timer cancelled")
				return nil
			}
		} else {
			r.Close()
		}
		if err == nil {
			err = fmt.Errorf("mstpd %q", strings.TrimSpace(out))
		}
		last = err
	}
	return fmt.Errorf("spanning tree not confirmed: %v", last)
}

// l2Wait is the pause before each confirmation attempt (tests set it to 0).
func (c *Collector) l2Wait() time.Duration {
	if c.L2Wait != nil {
		return *c.L2Wait
	}
	return 5 * time.Second
}

func l2Summary(in l2Inst, want l2Conf, confChanged, arm bool) string {
	var parts []string
	if in.Conf == nil {
		parts = append(parts, "install disunified-l2d")
	}
	if confChanged {
		guard := "off"
		if want.LoopGuard.Enabled {
			guard = fmt.Sprintf("%d ports", len(want.LoopGuard.Ports))
		}
		stp := "off"
		if want.STP.Enabled {
			stp = fmt.Sprintf("%s priority %d", want.STP.Version, want.STP.Priority)
		}
		parts = append(parts, fmt.Sprintf("config: spanning tree %s, loop protection %s", stp, guard))
	}
	if arm {
		parts = append(parts, fmt.Sprintf("first enable under a %s dead-man timer", l2DeadmanAfter))
	}
	if len(parts) == 0 {
		parts = append(parts, "update disunified-l2d")
	}
	return strings.Join(parts, "; ")
}

// installFile writes body to path through a temporary file (base64 on the
// command line keeps quoting out of it; nothing here is secret).
func installFile(path, mode, body string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(body))
	return fmt.Sprintf("t=$(mktemp) && echo %s | base64 -d > \"$t\" && sudo install -D -m %s \"$t\" %s && rm -f \"$t\"", b64, mode, path)
}

// mstpdVersion is the upstream release build-mstpd.sh fetches.
const mstpdVersion = "0.2.0"

// mstpdBuildHostScript starts the build in a transient unit and returns:
// the build outlives the SSH session and the apply budget, and the next
// collects read its outcome (collect.py: l2d.build, l2d.mstpd).
func mstpdBuildHostScript() string {
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "sudo mkdir -p %s\n", mstpdBuildIn)
	b.WriteString(installFile(mstpdBuildIn+"/build.sh", "0755", buildMstpdScript) + "\n")
	host := strings.Join([]string{
		"#!/bin/sh",
		"# disunified: builds mstpd for this switch (docs/drivers/sonic.md 11.5)",
		"d=" + mstpdBuildIn,
		"echo running > $d/status",
		"if docker run --rm --network host -v $d:/out " + alpineImage + " sh /out/build.sh > $d/build.log 2>&1 &&",
		"   install -m 0755 $d/mstpd $d/mstpctl /usr/local/sbin/; then",
		"  echo ok > $d/status",
		"else",
		"  echo \"failed: $(tail -n 1 $d/build.log)\" > $d/status",
		"fi",
		// by its digest: a name:tag@digest reference removes nothing (seen live)
		"docker rmi " + alpineDigestRef + " >/dev/null 2>&1 || true",
		"",
	}, "\n")
	b.WriteString(installFile(mstpdBuildIn+"/host.sh", "0755", host) + "\n")
	b.WriteString("sudo systemd-run --quiet --collect --unit=disunified-mstpd-build /bin/sh " + mstpdBuildIn + "/host.sh\n")
	return b.String()
}

// alpineImage is the build container, pinned by digest; alpineDigestRef is
// the same image as `docker rmi` takes it.
const (
	alpineImage     = "alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	alpineDigestRef = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
)

// --- read side ---

// l2System fills the bridge-level STP fields from l2d's STATE_DB table.
func l2System(sys *devicemodel.System, db map[string]table, now time.Time) bool {
	b := db["l2_bridge"]["DUI_STP_TABLE|Bridge"]
	if b["enabled"] != "yes" {
		return false
	}
	sys.STPMode = strings.ToLower(b["version"])
	if sys.STPMode == "" {
		sys.STPMode = "rstp"
	}
	if prio, mac, ok := bridgeID(b["bridge_id"]); ok {
		sys.STPPriority = prio
		_ = mac
	}
	if _, mac, ok := bridgeID(b["root_id"]); ok {
		sys.STPRoot = mac
	}
	sys.STPTopologyChanges = atoi(b["topology_change_count"])
	if sys.STPTopologyChanges > 0 {
		if secs, err := strconv.Atoi(b["time_since_topology_change"]); err == nil {
			sys.STPLastTopologyChange = now.Add(-time.Duration(secs) * time.Second).Truncate(time.Second)
		}
	}
	return true
}

// bridgeID parses mstpd's bridge id, "8.000.0C:48:C6:80:32:7E": the
// priority nibble (x4096), the system id extension, the MAC.
func bridgeID(s string) (int, string, bool) {
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 || len(parts[2]) != 17 {
		return 0, "", false
	}
	n, err := strconv.ParseUint(parts[0], 16, 8)
	if err != nil {
		return 0, "", false
	}
	return int(n) * 4096, strings.ToLower(parts[2]), true
}

// l2Ports fills each port's STP state from l2d's STATE_DB tables: the
// kernel bridge port is the interface, or the PortChannel it is a member of.
func l2Ports(out []cagePort, db map[string]table) {
	sp := db["l2_port"].byName("|")
	guard := db["l2_guard"].byName("|")
	lags := map[string]string{}
	for k := range db["cfg_lag_member"] {
		parts := strings.Split(k, "|")
		if len(parts) == 3 {
			lags[parts[2]] = parts[1]
		}
	}
	for i := range out {
		p := &out[i].Port
		if len(p.Interfaces) == 0 {
			continue
		}
		name := p.Interfaces[0]
		if lag := lags[name]; lag != "" {
			name = lag
		}
		if g := guard[name]; g["state"] == "blocked" {
			p.Fault = "errdisabled: loop-protect (" + g["mac"] + " flapping with " + g["peer"] + ")"
		}
		s, ok := sp[name]
		if !ok {
			continue
		}
		switch st := strings.ToLower(s["state"]); st {
		case "forwarding", "learning", "discarding", "blocking", "listening", "disabled":
			p.STPState = st
		}
		switch r := strings.ToLower(s["role"]); r {
		case "root", "designated", "alternate", "backup", "disabled":
			p.STPRole = r
		}
		p.STPPathCost = atoi(s["external_port_cost"])
		if p.STPRole == "disabled" {
			// mstpd: a down port is role Disabled, discarding, at its
			// default cost; the ECS reports it disabled at cost 0.
			p.STPState, p.STPPathCost = "disabled", 0
		}
		p.BPDUGuard = s["bpdu_guard_port"] == "yes"
		p.STPOperEdge = s["oper_edge_port"] == "yes"
		p.STPEdge = s["bpdu_filter_port"] == "yes" && s["admin_edge_port"] == "yes"
		p.Health.STPChanges = atoi(s["num_transition_fwd"]) + atoi(s["num_transition_blk"])
		if s["bpdu_guard_error"] == "yes" {
			p.Fault = "errdisabled: bpduguard"
		}
	}
}
