// Package informloop drives the device side of the UniFi inform protocol for
// one emulated switch: it POSTs an inform packet to the controller on an
// interval, feeds every reply through the unifi-emu session state machine,
// and records every reply to disk so a controller behaviour none of the
// prior-art projects ever saw becomes visible instead of silently ignored.
//
// The wire format, crypto, and adoption state machine come from
// github.com/jamesbraid/unifi-emu/inform (MIT). This package owns only the
// HTTP loop and the recording.
package informloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/device"
	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
	"github.com/TechBlueprints/switch-to-unifi/internal/unificfg"
	"github.com/jamesbraid/unifi-emu/inform"
)

// State is the device-side view of the adoption lifecycle.
type State int

const (
	StatePending State = iota
	StateAdopting
	StateConnected
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "PENDING"
	case StateAdopting:
		return "ADOPTING"
	case StateConnected:
		return "CONNECTED"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// Config is everything the loop needs beyond the session.
type Config struct {
	Interval       time.Duration         // initial inform cadence; the controller may change it
	RecordDir      string                // "" disables reply recording
	Collector      switchmodel.Collector // nil = report the model's synthetic table
	CollectTimeout time.Duration
	Logger         *log.Logger

	// Controller applies controller-pushed port config to the switch. nil =
	// read-only: pushes are accepted (cfgversion echoed) but never written.
	Controller switchmodel.Controller
	// ControlPorts restricts writes to these port indexes; nil = every port.
	ControlPorts map[int]bool
	// ControlIGMP lets the controller's per-network IGMP snooping setting
	// drive the switch. Off by default: UniFi defaults snooping off per
	// network while EOS defaults it on, so the first apply would turn
	// snooping off on every VLAN not enabled in UniFi.
	ControlIGMP bool
	// ControlNTP / ControlSyslog let the controller's NTP servers and
	// remote syslog host replace the switch's (off by default: they remove
	// servers the switch had that the controller does not list).
	ControlNTP    bool
	ControlSyslog bool
	// JumboAlwaysOn: the switch forwards jumbo frames unconditionally, so a
	// controller asking for jumbo off is logged rather than applied.
	JumboAlwaysOn bool
	// ControlReboot: the controller's reboot command really reloads the
	// switch (off by default: it is emulated).
	ControlReboot bool
	// ControlSNMP: the controller's SNMP v1/v2c community is configured on
	// the switch (read-only) and removed when UniFi turns SNMP off.
	ControlSNMP bool
	// ControlSSHKeys: install the SSH keys the controller pushes on the switch.
	ControlSSHKeys bool
	// AllowInitialChanges lets the first push after adoption change ports.
	// Off by default: a freshly adopted device has no port config in the
	// controller, so its first push says "every port at its defaults" and
	// would strip whatever the switch had. The loop holds such a push (the
	// device keeps reporting the old cfgversion, the controller keeps
	// re-sending) until a push arrives that changes nothing — which is what
	// seeding the controller from the switch on adoption produces — or the
	// operator sets the ports. Needs a driver that implements
	// switchmodel.Planner; others apply as before.
	AllowInitialChanges bool
	// DefaultPortNames maps port_idx to the names that mean "unedited"; a
	// UniFi port name in that set is written as "no description".
	DefaultPortNames map[int][]string
	// OnHeld runs each time a first push is held (see AllowInitialChanges),
	// with the current snapshot: the bridge retries seeding the controller.
	OnHeld func(snap *switchmodel.Snapshot)
	// OnConnected runs once each time the adoption handshake completes
	// (state -> CONNECTED), with the current snapshot: first-provision work
	// such as naming the device and its ports through the REST API belongs
	// here, because a device adopted after the bridge started has no other
	// trigger (found 2026-09-20: a re-adopted switch stayed "USW Leaf").
	OnConnected func(snap *switchmodel.Snapshot)

	// OnLayoutChange runs after a collect whose port layout (lane counts,
	// interface names) differs from the previous one — a cage split or joined.
	OnLayoutChange func(snap *switchmodel.Snapshot)
	// SwitchHost is the address the bridge reaches the switch at, for the
	// out-of-band management warning ("" = unknown).
	SwitchHost string
	// GatewayIP is the controller/gateway address; the switch's ARP entry
	// for it is reported as gateway_mac ("" = skip).
	GatewayIP string

	// OnSystemCfg runs with every system_cfg the controller pushes (and the
	// last applied one at startup): the SSH gateway takes its credentials
	// from it.
	OnSystemCfg func(cfg *unificfg.Config)
}

// Loop is one device's inform loop.
type Loop struct {
	desc    inform.Descriptor
	cfg     Config
	client  *http.Client
	session *device.Session

	collectFailures int
	applyFailures   int
	reconciledOnce  bool
	layoutSig       string
	pendingCycles   []int
	pendingReboot   bool
	warnedVersion   string
	oobWarned       string // last out-of-band warning state, to log on change only
	uplinkPort      int    // the port last marked as uplink from the snapshot
	everApplied     bool   // a system_cfg has been applied to this device (this run or a previous one)
	heldVersion     string // the first push being held, to log once
	// freshPorts are ports that appeared (a new guest, a new interface) since
	// the last applied config: the controller knows nothing about them yet,
	// so its config for them is the defaults. They are not written until a
	// push matches their live state (see withholdFreshPorts).
	freshPorts   map[int]string // port -> interface name, for the log
	freshWarned  map[int]bool
	layoutByPort map[int]string
	faultSig     string

	mu         sync.Mutex
	state      State
	interval   time.Duration
	lastStatus int
	statusRun  int
	lastMgmt   string

	recorder *recorder
}

// New builds a loop driving sess. A session resumed from persisted state
// that is already adopted starts CONNECTED; the controller decides on the
// first reply whether it still agrees.
func New(desc inform.Descriptor, sess *device.Session, cfg Config) (*Loop, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.CollectTimeout <= 0 {
		cfg.CollectTimeout = 20 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	l := &Loop{
		desc:     desc,
		cfg:      cfg,
		client:   &http.Client{Timeout: 30 * time.Second},
		session:  sess,
		state:    StatePending,
		interval: cfg.Interval,
	}
	if sess.Adopted() {
		l.state = StateConnected
	}
	if _, _, ok := sess.Applied(); ok {
		l.everApplied = true
	}
	if cfg.RecordDir != "" {
		r, err := newRecorder(cfg.RecordDir, desc.MAC)
		if err != nil {
			return nil, err
		}
		l.recorder = r
	}
	return l, nil
}

// State returns the current adoption state.
func (l *Loop) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// Run informs until ctx is cancelled. The interval is re-read every cycle
// because the controller can change it through a reply.
func (l *Loop) Run(ctx context.Context) {
	l.cfg.Logger.Printf("[%s] informing %s as %s (%s), state %s, gcm=%v",
		l.desc.MAC, l.session.InformURL(), l.desc.Model, l.desc.ModelDisplay, l.State(), l.session.UseAESGCM())
	l.reconcile(ctx)
	for {
		l.informOnce(ctx)
		l.mu.Lock()
		interval := l.interval
		l.mu.Unlock()
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			if l.recorder != nil {
				l.recorder.Close()
			}
			return
		case <-t.C:
		}
	}
}

// informOnce sends one inform and applies the reply.
//
// HTTP 404 is the normal answer while pending: the controller has nothing
// queued for a device nobody has adopted. It is logged on transition only.
// The ADOPTING -> CONNECTED transition is taken when a reply arrives to an
// inform that was *sent* adopted, so the state is sampled before Apply.
func (l *Loop) informOnce(ctx context.Context) {
	l.collect(ctx)
	now := time.Now()
	enc, err := l.session.EncodeInform(now)
	if l.cfg.RecordDir != "" {
		// The last payload as sent, for diagnosis (overwritten every cycle).
		_ = os.WriteFile(filepath.Join(l.cfg.RecordDir, "payload-last.json"), l.session.BuildPayload(now), 0o600)
	}
	url := l.session.InformURL()
	key := l.session.AuthKey()
	wasAdopted := l.session.Adopted()
	wasGCM := l.session.UseAESGCM()
	if err != nil {
		l.cfg.Logger.Printf("[%s] encode inform: %v", l.desc.MAC, err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(enc))
	if err != nil {
		l.cfg.Logger.Printf("[%s] build request: %v", l.desc.MAC, err)
		return
	}
	req.Header.Set("Content-Type", "application/x-binary")
	req.Header.Set("User-Agent", "AirControl Agent v1.0")

	resp, err := l.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			l.cfg.Logger.Printf("[%s] POST %s: %v", l.desc.MAC, url, err)
			l.record(record{At: now, Error: err.Error(), SentAdopted: wasAdopted, SentGCM: wasGCM})
		}
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		l.cfg.Logger.Printf("[%s] read reply: %v", l.desc.MAC, err)
		return
	}

	rec := record{At: now, Status: resp.StatusCode, BodyLen: len(raw), SentAdopted: wasAdopted, SentGCM: wasGCM}
	l.noteStatus(resp.StatusCode)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		l.record(rec)
		return
	case resp.StatusCode != http.StatusOK:
		rec.RawBody = string(raw)
		l.cfg.Logger.Printf("[%s] inform got %s: %s", l.desc.MAC, resp.Status, raw)
		l.record(rec)
		return
	case len(raw) == 0:
		l.record(rec)
		return
	}

	dec, err := inform.Decode(raw, key)
	if err != nil {
		rec.Error = "decode: " + err.Error()
		rec.RawBodyHex = fmt.Sprintf("%x", raw)
		l.cfg.Logger.Printf("[%s] decode reply: %v", l.desc.MAC, err)
		l.record(rec)
		return
	}
	if json.Valid(dec.Payload) {
		rec.Payload = json.RawMessage(dec.Payload)
	} else {
		rec.RawBody = string(dec.Payload)
	}

	effects := l.session.Apply(time.Now(), dec.Payload)
	l.mu.Lock()
	wasAdopting := l.state == StateAdopting
	var lines []string
	for _, e := range effects {
		rec.Effects = append(rec.Effects, effectRecord{Kind: effectName(e.Kind), Text: e.Text, Interval: e.Interval.String()})
		switch e.Kind {
		case inform.EffectAdoptingViaSetAdopt:
			l.state = StateAdopting
			lines = append(lines, fmt.Sprintf("set-adopt received, key rotated, informing %s, now ADOPTING", e.Text))
		case inform.EffectAdoptingViaMgmtCfg:
			l.state = StateAdopting
			lines = append(lines, "authkey adopted from mgmt_cfg, now ADOPTING")
		case inform.EffectFactoryReset:
			l.state = StatePending
			lines = append(lines, "setdefault received, factory reset, back to PENDING")
		case inform.EffectRebooted:
			if l.cfg.ControlReboot {
				lines = append(lines, "reboot requested: reloading the switch")
				l.pendingReboot = true
			} else {
				lines = append(lines, "reboot requested (emulated; -control-reboot to really reload)")
			}
		case inform.EffectUpgraded:
			lines = append(lines, fmt.Sprintf("upgrade to %q requested (emulated reboot)", e.Text))
		case inform.EffectInterval:
			if e.Interval > 0 && e.Interval != l.interval {
				l.interval = e.Interval
				lines = append(lines, fmt.Sprintf("inform interval now %s", e.Interval))
			}
		case inform.EffectMgmtCfg:
			if e.Text != l.lastMgmt {
				l.lastMgmt = e.Text
				lines = append(lines, fmt.Sprintf("mgmt_cfg: %q", maskAuthKey(e.Text)))
			}
		case inform.EffectUnknownCmd:
			lines = append(lines, fmt.Sprintf("UNHANDLED cmd %q", e.Text))
		case inform.EffectUnknownType:
			lines = append(lines, fmt.Sprintf("UNHANDLED _type %q", e.Text))
		case inform.EffectDecodeError:
			lines = append(lines, fmt.Sprintf("undecodable reply payload: %s", e.Text))
		case device.EffectSystemCfg:
			lines = append(lines, fmt.Sprintf("system_cfg received, cfgversion %s, pending apply", e.Text))
		case device.EffectLocate:
			lines = append(lines, fmt.Sprintf("locate %s (reported as locating; this switch has no LED control)", e.Text))
		case device.EffectPortCycle:
			lines = append(lines, fmt.Sprintf("port-cycle requested for port %s", e.Text))
			if idx, err := strconv.Atoi(e.Text); err == nil {
				pending := l.pendingCycles
				l.pendingCycles = append(pending, idx)
			}
		}
	}
	connectedNow := false
	if wasAdopting && l.session.Adopted() && l.state == StateAdopting {
		l.state = StateConnected
		connectedNow = true
		lines = append(lines, "adoption handshake complete -> CONNECTED")
	}
	rec.StateAfter = l.state.String()
	l.mu.Unlock()

	for _, s := range lines {
		l.cfg.Logger.Printf("[%s] %s", l.desc.MAC, s)
	}
	l.record(rec)
	if connectedNow && l.cfg.OnConnected != nil {
		l.cfg.OnConnected(l.session.Snapshot())
	}
	l.cyclePorts(ctx)
	if !l.applyPending(ctx) {
		l.reconcile(ctx)
	}
}

// cyclePorts runs queued port-cycle (and reboot) commands through the driver.
func (l *Loop) cyclePorts(ctx context.Context) {
	l.mu.Lock()
	queue := l.pendingCycles
	l.pendingCycles = nil
	reboot := l.pendingReboot
	l.pendingReboot = false
	l.mu.Unlock()
	if reboot {
		if rb, ok := l.cfg.Controller.(switchmodel.Rebooter); ok {
			cctx, cancel := context.WithTimeout(ctx, l.cfg.CollectTimeout)
			err := rb.Reboot(cctx)
			cancel()
			if err != nil {
				l.cfg.Logger.Printf("[%s] reboot: %v", l.desc.MAC, err)
			} else {
				l.cfg.Logger.Printf("[%s] reboot: switch reload issued", l.desc.MAC)
			}
		}
	}
	if len(queue) == 0 {
		return
	}
	pc, ok := l.cfg.Controller.(switchmodel.PortCycler)
	if !ok {
		l.cfg.Logger.Printf("[%s] port-cycle: driver cannot bounce ports; ignored", l.desc.MAC)
		return
	}
	for _, idx := range queue {
		cctx, cancel := context.WithTimeout(ctx, l.cfg.CollectTimeout)
		err := pc.CyclePort(cctx, idx)
		cancel()
		if err != nil {
			l.cfg.Logger.Printf("[%s] port-cycle %d: %v", l.desc.MAC, idx, err)
		} else {
			l.cfg.Logger.Printf("[%s] port-cycle %d: done", l.desc.MAC, idx)
		}
	}
}

// reconcile re-applies the last applied system_cfg — at startup and after
// every inform that carried no new push — so the switch converges to the
// controller's intent even if it drifted while the bridge was down, the
// bridge's translation changed, or a breakout split created lanes that
// need the cage's VLAN/FEC/storm config. ApplyPorts is a diff, so a
// converged switch costs nothing and logs nothing.
func (l *Loop) reconcile(ctx context.Context) {
	if l.cfg.Controller == nil {
		return
	}
	ver, text, ok := l.session.Applied()
	if !ok {
		return
	}
	if !l.reconciledOnce && l.cfg.OnSystemCfg != nil {
		l.cfg.OnSystemCfg(unificfg.Parse(text))
	}
	cctx, cancel := context.WithTimeout(ctx, l.cfg.CollectTimeout)
	defer cancel()
	l.ensureVLANs(cctx, text)
	l.applySwitchSettings(cctx, text)
	l.installSSHKeys(cctx, text)
	desired := l.withholdFreshPorts(l.desiredPorts(text))
	changed, err := l.cfg.Controller.ApplyPorts(cctx, desired)
	if err != nil {
		l.cfg.Logger.Printf("[%s] reconcile system_cfg %s: %v", l.desc.MAC, ver, err)
		return
	}
	if changed > 0 || !l.reconciledOnce {
		l.cfg.Logger.Printf("[%s] reconciled system_cfg %s: %d of %d ports changed", l.desc.MAC, ver, changed, len(desired))
	}
	l.reconciledOnce = true
}

// desiredPorts translates a system_cfg into per-port intent for the ports
// this bridge is allowed to write.
func (l *Loop) desiredPorts(text string) []switchmodel.PortDesired {
	cfg := unificfg.Parse(text)
	var desired []switchmodel.PortDesired
	for _, idx := range cfg.PortIndexes() {
		if l.cfg.ControlPorts != nil && !l.cfg.ControlPorts[idx] {
			continue
		}
		p := cfg.Ports[idx]
		d := switchmodel.PortDesired{Index: idx, Enabled: p.Enabled}
		if !l.isDefaultName(idx, p.Name) {
			d.Description = p.Name
		}
		if !p.AutoNeg && p.SpeedMbps > 0 {
			d.SpeedMbps = p.SpeedMbps
		}
		d.VLANSet = true
		if p.VLANExplicit {
			d.NativeVLAN = p.NativeVLAN
			d.TaggedVLANs = p.TaggedVLANs
		} else {
			d.NativeVLAN = 1
			d.TaggedAll = true
		}
		switch p.FEC {
		case "cl-91", "rs-fec":
			f := switchmodel.FECRS
			d.FEC = &f
		case "cl-74", "fc-fec":
			f := switchmodel.FECFC
			d.FEC = &f
		case "disabled", "none", "off":
			f := switchmodel.FECDisabled
			d.FEC = &f
		}
		if p.StormCtrl.Enabled && p.StormCtrl.Type != "rate" {
			spec := &switchmodel.StormControlSpec{}
			if p.StormCtrl.Bcast >= 0 {
				v := float64(p.StormCtrl.Bcast)
				spec.BroadcastPct = &v
			}
			if p.StormCtrl.Mcast >= 0 {
				v := float64(p.StormCtrl.Mcast)
				spec.MulticastPct = &v
			}
			if p.StormCtrl.Ucast >= 0 {
				v := float64(p.StormCtrl.Ucast)
				spec.UnknownUnicastPct = &v
			}
			d.StormCtrl = spec
		}
		d.BPDUGuard = p.BPDUGuard
		d.STPEdge = !p.STPPortMode
		switch p.OpMode {
		case "aggregate":
			d.LAG = p.LAG
		case "mirror":
			d.MirrorSource = p.MirrorPort
		}
		desired = append(desired, d)
	}
	return desired
}

// unsupportedFeatures are config keys no driver honours; when the
// controller sends them the operator gets a log line naming the port and
// feature, because the UI otherwise looks like it worked.
var unsupportedFeatures = map[string]string{
	".isolation":        "port isolation",
	".dot1x":            "802.1X",
	".egress_rate":      "egress rate limit",
	".port_security":    "port security (MAC restriction)",
	".poe":              "PoE",
	".voice_vlan":       "voice VLAN",
	".lldpmed.opmode=":  "", // handled below (informational)
	"switch.dot1x":      "802.1X",
	"switch.dhcp_snoop": "DHCP snooping",
}

func (l *Loop) warnUnsupported(ver, text string) {
	cfg := unificfg.Parse(text)
	feats := map[string]string{}
	for k, v := range unsupportedFeatures {
		if v != "" {
			feats[k] = v
		}
	}
	for feature, keys := range cfg.Unsupported(feats) {
		l.cfg.Logger.Printf("[%s] UNSUPPORTED by this switch: %s requested in system_cfg %s (%s) — the UI setting has no effect", l.desc.MAC, feature, ver, strings.Join(keys, ", "))
	}
	if !cfg.JumboFrames && l.cfg.JumboAlwaysOn {
		l.cfg.Logger.Printf("[%s] NOTE: controller has jumbo frames off; this switch always forwards jumbo frames at layer 2 and cannot turn that off", l.desc.MAC)
	}
	for _, idx := range cfg.PortIndexes() {
		if p := cfg.Ports[idx]; p.LLDPMED != nil && !*p.LLDPMED {
			l.cfg.Logger.Printf("[%s] NOTE: LLDP-MED disabled on port %d in the controller; EOS 4.26 has no per-port LLDP-MED toggle, left as is", l.desc.MAC, idx)
			break
		}
	}
}

// desiredSwitch translates the switch-wide keys.
func (l *Loop) desiredSwitch(text string) switchmodel.SwitchDesired {
	cfg := unificfg.Parse(text)
	d := switchmodel.SwitchDesired{
		STPSet: cfg.STP.Set, STPEnabled: cfg.STP.Enabled, STPMode: cfg.STP.Version, STPPriority: cfg.STP.Priority,
		ManageNTP: l.cfg.ControlNTP, NTPServers: cfg.NTPServers,
		ManageSyslog: l.cfg.ControlSyslog, SyslogHosts: cfg.SyslogHosts,
		DHCPSnooping: cfg.DHCPSnoop,
	}
	if cfg.SNMP != nil && l.cfg.ControlSNMP {
		d.SNMPSet = true
		if cfg.SNMP.Enabled && strings.HasPrefix(cfg.SNMP.Version, "1") {
			d.SNMPCommunity = cfg.SNMP.Community
		}
	}
	if l.cfg.ControlIGMP && len(cfg.IGMPSnooping) > 0 {
		d.IGMPSnooping = cfg.IGMPSnooping
		// VLANs the controller lists without an igmp key are "off" in UniFi.
		for _, id := range cfg.VLANIDs() {
			if _, ok := d.IGMPSnooping[id]; !ok {
				d.IGMPSnooping[id] = false
			}
		}
	}
	return d
}

// installSSHKeys puts the controller's pushed keys on the switch when enabled.
func (l *Loop) installSSHKeys(ctx context.Context, text string) {
	if !l.cfg.ControlSSHKeys {
		return
	}
	inst, ok := l.cfg.Controller.(switchmodel.SSHKeyInstaller)
	if !ok {
		return
	}
	cfg := unificfg.Parse(text)
	keys := make([]switchmodel.SSHKey, 0, len(cfg.SSHKeys))
	for _, k := range cfg.SSHKeys {
		keys = append(keys, switchmodel.SSHKey{Type: k.Type, Value: k.Value, Comment: k.Comment})
	}
	n, err := inst.InstallSSHKeys(ctx, keys)
	if err != nil {
		l.cfg.Logger.Printf("[%s] install controller SSH keys: %v", l.desc.MAC, err)
		return
	}
	if n > 0 {
		l.cfg.Logger.Printf("[%s] installed %d controller SSH keys on the switch (%d pushed)", l.desc.MAC, n, len(keys))
	}
}

// applySwitchSettings writes STP/IGMP settings; errors are logged, and the
// port apply still proceeds (they are independent).
func (l *Loop) applySwitchSettings(ctx context.Context, text string) {
	sc, ok := l.cfg.Controller.(switchmodel.SwitchController)
	if !ok {
		return
	}
	n, err := sc.ApplySwitch(ctx, l.desiredSwitch(text))
	if err != nil {
		l.cfg.Logger.Printf("[%s] apply switch settings: %v", l.desc.MAC, err)
		return
	}
	if n > 0 {
		l.cfg.Logger.Printf("[%s] applied %d switch-level settings (STP/IGMP)", l.desc.MAC, n)
	}
}

// ensureVLANs creates the site's VLANs on the switch before ports reference
// them. Errors are reported, not fatal: ports whose VLANs exist still apply.
func (l *Loop) ensureVLANs(ctx context.Context, text string) {
	vc, ok := l.cfg.Controller.(switchmodel.VLANController)
	if !ok {
		return
	}
	ids := unificfg.Parse(text).VLANIDs()
	n, err := vc.EnsureVLANs(ctx, ids)
	if err != nil {
		l.cfg.Logger.Printf("[%s] ensure vlans: %v", l.desc.MAC, err)
		return
	}
	if n > 0 {
		l.cfg.Logger.Printf("[%s] created %d VLANs on the switch", l.desc.MAC, n)
	}
}

func (l *Loop) isDefaultName(idx int, name string) bool {
	if name == "" {
		return true
	}
	for _, d := range l.cfg.DefaultPortNames[idx] {
		if name == d {
			return true
		}
	}
	return false
}

// applyPending writes a controller config push to the switch, then lets the
// session report its cfgversion. A failure leaves the push pending: the
// device keeps reporting the old cfgversion, the controller keeps re-sending,
// and the next inform retries. Without a Controller the push is accepted
// as-is (read-only mode). Runs after every inform so a push persisted
// across a restart is applied too.
func (l *Loop) applyPending(ctx context.Context) bool {
	ver, text, ok := l.session.Pending()
	if !ok {
		return false
	}
	if l.cfg.Controller == nil {
		l.cfg.Logger.Printf("[%s] system_cfg %s accepted without applying (read-only mode)", l.desc.MAC, ver)
		if err := l.session.MarkApplied(ver); err != nil {
			l.cfg.Logger.Printf("[%s] persist state: %v", l.desc.MAC, err)
		}
		return true
	}
	if l.warnedVersion != ver {
		l.warnedVersion = ver
		l.warnUnsupported(ver, text)
		if l.cfg.OnSystemCfg != nil {
			l.cfg.OnSystemCfg(unificfg.Parse(text))
		}
	}
	desired := l.desiredPorts(text)
	if held := l.holdInitialPush(ver, desired); held {
		return true
	}
	desired = l.withholdFreshPorts(desired)
	cctx, cancel := context.WithTimeout(ctx, l.cfg.CollectTimeout)
	defer cancel()
	l.ensureVLANs(cctx, text)
	l.applySwitchSettings(cctx, text)
	l.installSSHKeys(cctx, text)
	changed, err := l.cfg.Controller.ApplyPorts(cctx, desired)
	if err != nil {
		l.applyFailures++
		if l.applyFailures == 1 || l.applyFailures%10 == 0 {
			l.cfg.Logger.Printf("[%s] apply system_cfg %s (%d attempts): %v", l.desc.MAC, ver, l.applyFailures, err)
		}
		return true
	}
	l.applyFailures = 0
	l.everApplied = true
	l.cfg.Logger.Printf("[%s] applied system_cfg %s to the switch: %d of %d ports changed", l.desc.MAC, ver, changed, len(desired))
	if err := l.session.MarkApplied(ver); err != nil {
		l.cfg.Logger.Printf("[%s] persist state: %v", l.desc.MAC, err)
	}
	return true
}

// holdInitialPush keeps the first push after adoption from changing ports
// (see Config.AllowInitialChanges). It reports whether the push is held.
func (l *Loop) holdInitialPush(ver string, desired []switchmodel.PortDesired) bool {
	if l.everApplied || l.cfg.AllowInitialChanges {
		return false
	}
	planner, ok := l.cfg.Controller.(switchmodel.Planner)
	if !ok {
		return false
	}
	changed := planner.PlanPorts(desired)
	if len(changed) == 0 {
		return false
	}
	if l.heldVersion != ver {
		l.heldVersion = ver
		l.cfg.Logger.Printf("[%s] HOLDING the first push after adoption (system_cfg %s): it would change %d ports %v, and a new device's controller config is only the defaults. Not applied. Seed the controller from the switch (automatic with api_url), or set these ports in the UI; the push that changes nothing goes through. control.allow_initial_changes: true overrides.", l.desc.MAC, ver, len(changed), changed)
	}
	if l.cfg.OnHeld != nil {
		l.cfg.OnHeld(l.session.Snapshot())
	}
	return true
}

// withholdFreshPorts drops from desired the fresh ports (see Loop.freshPorts)
// that the plan says would change: the controller has not been told about
// them yet (the bridge seeds it on layout change) and its defaults would
// overwrite what they have. A fresh port whose desired state matches its
// live state stops being fresh. Needs a Planner; drivers without one write
// as before. AllowInitialChanges disables the withholding too.
func (l *Loop) withholdFreshPorts(desired []switchmodel.PortDesired) []switchmodel.PortDesired {
	if len(l.freshPorts) == 0 || l.cfg.AllowInitialChanges {
		return desired
	}
	planner, ok := l.cfg.Controller.(switchmodel.Planner)
	if !ok {
		return desired
	}
	wouldChange := map[int]bool{}
	for _, idx := range planner.PlanPorts(desired) {
		wouldChange[idx] = true
	}
	out := desired[:0:0]
	for _, d := range desired {
		name, fresh := l.freshPorts[d.Index]
		if !fresh {
			out = append(out, d)
			continue
		}
		if !wouldChange[d.Index] {
			delete(l.freshPorts, d.Index) // controller and switch agree: an ordinary port from now on
			delete(l.freshWarned, d.Index)
			out = append(out, d)
			continue
		}
		if !l.freshWarned[d.Index] {
			if l.freshWarned == nil {
				l.freshWarned = map[int]bool{}
			}
			l.freshWarned[d.Index] = true
			l.cfg.Logger.Printf("[%s] port %d (%s) is new since the controller's config and would be changed by it; not written until the controller's config for it matches the switch (seeded automatically with api_url, or set it in the UI). control.allow_initial_changes: true overrides.", l.desc.MAC, d.Index, name)
		}
	}
	return out
}

// collect refreshes the session's snapshot from the switch. A failure keeps
// the previous snapshot (the controller sees stale-but-plausible data for one
// interval) and is logged on the first failure and every tenth after that.
func (l *Loop) collect(ctx context.Context) {
	if l.cfg.Collector == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, l.cfg.CollectTimeout)
	defer cancel()
	snap, err := l.cfg.Collector.Collect(cctx)
	if err != nil {
		l.collectFailures++
		if l.collectFailures == 1 || l.collectFailures%10 == 0 {
			l.cfg.Logger.Printf("[%s] collect from switch (%d in a row): %v", l.desc.MAC, l.collectFailures, err)
		}
		return
	}
	if l.collectFailures > 0 {
		l.cfg.Logger.Printf("[%s] collect from switch recovered after %d failures", l.desc.MAC, l.collectFailures)
		l.collectFailures = 0
	}
	if snap.System.GatewayMAC == "" && l.cfg.GatewayIP != "" {
		snap.System.GatewayMAC = snap.System.ARP[l.cfg.GatewayIP]
	}
	l.session.SetGatewayIP(l.cfg.GatewayIP)
	l.session.SetSnapshot(snap)
	if up := snap.UplinkPort(); up > 0 && up != l.uplinkPort {
		if l.uplinkPort != 0 {
			l.cfg.Logger.Printf("[%s] uplink is now port %d (was %d)", l.desc.MAC, up, l.uplinkPort)
		}
		l.uplinkPort = up
		l.session.SetUplinkPort(up)
	}
	l.warnOOB(snap)
	var faults []string
	for _, p := range snap.Ports {
		if p.Fault != "" {
			faults = append(faults, fmt.Sprintf("%d(%s)", p.Index, p.Fault))
		}
	}
	if fs := strings.Join(faults, " "); fs != l.faultSig {
		l.faultSig = fs
		if fs != "" {
			l.cfg.Logger.Printf("[%s] PORT FAULTS on the switch (shown as down in UniFi): %s", l.desc.MAC, fs)
		} else {
			l.cfg.Logger.Printf("[%s] port faults cleared", l.desc.MAC)
		}
	}
	if sig := layoutSignature(snap); sig != l.layoutSig {
		changed := l.layoutSig != ""
		l.layoutSig = sig
		// A port's identity for freshness is its interface name plus the
		// interfaces behind it: a guest that migrates onto this node keeps
		// its port number and name but gains its tap here, and must be
		// treated as fresh — this device's controller config for the port
		// may differ from the guest's own.
		byPort := map[int]string{}
		for _, p := range snap.Ports {
			if p.IfName == "" {
				continue
			}
			byPort[p.Index] = p.IfName + "/" + strings.Join(p.Interfaces, ",")
		}
		if changed {
			var fresh []string
			for idx, name := range byPort {
				if name != "" && l.layoutByPort[idx] != name {
					if l.freshPorts == nil {
						l.freshPorts = map[int]string{}
					}
					l.freshPorts[idx] = name
					fresh = append(fresh, fmt.Sprintf("%d(%s)", idx, name))
				}
			}
			sort.Strings(fresh)
			l.cfg.Logger.Printf("[%s] port layout changed: new, changed or arrived ports %v are not written until the controller's config matches them", l.desc.MAC, fresh)
			if l.cfg.OnLayoutChange != nil {
				l.cfg.OnLayoutChange(snap)
			}
		}
		l.layoutByPort = byPort
	}
}

func layoutSignature(snap *switchmodel.Snapshot) string {
	var b strings.Builder
	for _, p := range snap.Ports {
		fmt.Fprintf(&b, "%d:%s:%d:%s;", p.Index, p.IfName, p.Lanes, strings.Join(p.Interfaces, ","))
	}
	return b.String()
}

// noteStatus logs HTTP status transitions, never every inform.
func (l *Loop) noteStatus(status int) {
	l.mu.Lock()
	prev, run := l.lastStatus, l.statusRun
	if status == l.lastStatus {
		l.statusRun++
		l.mu.Unlock()
		return
	}
	l.lastStatus, l.statusRun = status, 1
	l.mu.Unlock()
	switch {
	case status == http.StatusNotFound:
		l.cfg.Logger.Printf("[%s] inform: HTTP 404 (pending, nothing queued)", l.desc.MAC)
	case prev == 0:
		l.cfg.Logger.Printf("[%s] inform: HTTP %d", l.desc.MAC, status)
	default:
		l.cfg.Logger.Printf("[%s] inform: HTTP %d after %d x %d", l.desc.MAC, status, run, prev)
	}
}

func effectName(k inform.EffectKind) string {
	switch k {
	case inform.EffectAdoptingViaSetAdopt:
		return "adopting-via-set-adopt"
	case inform.EffectAdoptingViaMgmtCfg:
		return "adopting-via-mgmt-cfg"
	case inform.EffectFactoryReset:
		return "factory-reset"
	case inform.EffectRebooted:
		return "rebooted"
	case inform.EffectUpgraded:
		return "upgraded"
	case inform.EffectInterval:
		return "interval"
	case inform.EffectMgmtCfg:
		return "mgmt-cfg"
	case inform.EffectUnknownCmd:
		return "unknown-cmd"
	case inform.EffectUnknownType:
		return "unknown-type"
	case inform.EffectDecodeError:
		return "decode-error"
	case device.EffectSystemCfg:
		return "system-cfg"
	}
	return fmt.Sprintf("effect-%d", int(k))
}

// record is one line of the reply log: what was sent (adopted? GCM?), what
// came back, and what the session made of it.
type record struct {
	At          time.Time       `json:"at"`
	Status      int             `json:"status,omitempty"`
	BodyLen     int             `json:"body_len"`
	SentAdopted bool            `json:"sent_adopted"`
	SentGCM     bool            `json:"sent_gcm"`
	Error       string          `json:"error,omitempty"`
	RawBody     string          `json:"raw_body,omitempty"`
	RawBodyHex  string          `json:"raw_body_hex,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Effects     []effectRecord  `json:"effects,omitempty"`
	StateAfter  string          `json:"state_after,omitempty"`
}

type effectRecord struct {
	Kind     string `json:"kind"`
	Text     string `json:"text,omitempty"`
	Interval string `json:"interval,omitempty"`
}

func (l *Loop) record(r record) {
	if l.recorder == nil {
		return
	}
	if err := l.recorder.write(r); err != nil {
		l.cfg.Logger.Printf("[%s] record reply: %v", l.desc.MAC, err)
	}
}

// recorder appends NDJSON records to <dir>/<mac>.ndjson. Consecutive plain
// 404s are collapsed into a counter so a device that sits pending overnight
// does not write a line every ten seconds.
type recorder struct {
	f        *os.File
	enc      *json.Encoder
	quiet404 int
}

func newRecorder(dir, mac string) (*recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := filepath.Join(dir, filepath.Base(mac)+".ndjson")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &recorder{f: f, enc: json.NewEncoder(f)}, nil
}

func (r *recorder) write(rec record) error {
	if rec.Status == http.StatusNotFound && rec.Error == "" {
		r.quiet404++
		if r.quiet404 > 1 {
			return nil
		}
		return r.enc.Encode(rec)
	}
	if r.quiet404 > 1 {
		if err := r.enc.Encode(map[string]any{"at": rec.At, "collapsed_404s": r.quiet404 - 1}); err != nil {
			return err
		}
	}
	r.quiet404 = 0
	return r.enc.Encode(rec)
}

func (r *recorder) Close() {
	if r.quiet404 > 1 {
		_ = r.enc.Encode(map[string]any{"at": time.Now(), "collapsed_404s": r.quiet404 - 1})
	}
	_ = r.f.Close()
}

// warnOOB logs, loudly and on every change of state, when the switch's
// dedicated out-of-band management port is in use. A UniFi controller
// expects the management address in-band, behind the uplink: with the
// address on an OOB port it finds the switch's IP behind one UniFi switch and
// its LLDP identity behind another, and never places it in the topology
// (no Uplink, no Parent Device). An OOB port that is merely cabled is a
// milder risk (a second LLDP identity) and gets a note.
func (l *Loop) warnOOB(snap *switchmodel.Snapshot) {
	var msgs []string
	for _, o := range snap.System.OOBInterfaces {
		switch {
		case o.IP != "" && o.IP == l.cfg.SwitchHost:
			msgs = append(msgs, fmt.Sprintf("!!! the bridge reaches this switch through its out-of-band management port %s (%s). UniFi expects the management address in-band, behind the uplink; the controller will show no Uplink/Parent and will not place the switch in the topology. Move the address to a VLAN interface and point the bridge at it (docs/adding-a-switch.md)", o.Name, o.IP))
		case o.IP != "":
			msgs = append(msgs, fmt.Sprintf("!!! out-of-band management port %s carries address %s; the controller may locate the switch behind that port instead of its uplink. Prefer an in-band management address (docs/adding-a-switch.md)", o.Name, o.IP))
		case o.Up:
			msgs = append(msgs, fmt.Sprintf("NOTE: out-of-band management port %s is connected but addressless; keep LLDP off on it so the controller sees one identity for this switch", o.Name))
		}
	}
	state := strings.Join(msgs, "\n")
	if state == l.oobWarned {
		return
	}
	l.oobWarned = state
	for _, m := range msgs {
		l.cfg.Logger.Printf("[%s] %s", l.desc.MAC, m)
	}
}

var authKeyRe = regexp.MustCompile(`authkey=[0-9a-fA-F]{32}`)

// maskAuthKey hides the device auth key in logged mgmt_cfg text.
func maskAuthKey(s string) string { return authKeyRe.ReplaceAllString(s, "authkey=<key>") }
