package aristaeos

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// Shapes and translations for the feature keys the controller sends once
// the device claims capabilities: FEC, storm control, BPDU guard, STP mode
// and priority, IGMP snooping. Read-back comes from `show storm-control`,
// `show ip igmp snooping`, and the running config as JSON (verified on
// 4.26.14M; `show running-config | json` over SSH can carry raw control
// characters, eAPI serialises it cleanly).

type showStormControl struct {
	Interfaces map[string]struct {
		TrafficTypes map[string]struct {
			Level         int    `json:"level"` // hundredths of a percent when thresholdType == percentage
			ThresholdType string `json:"thresholdType"`
		} `json:"trafficTypes"`
	} `json:"interfaces"`
}

type showIGMPSnooping struct {
	State string `json:"igmpSnoopingState"`
	VLANs map[string]struct {
		State string `json:"igmpSnoopingState"`
	} `json:"vlans"`
}

// showRunningConfig is EOS's structured config: top-level command -> its
// sub-commands (a section) or null (a one-liner).
type showRunningConfig struct {
	Cmds map[string]*configSection `json:"cmds"`
}

type configSection struct {
	Cmds map[string]*configSection `json:"cmds"`
}

var (
	stormRe = regexp.MustCompile(`^storm-control (broadcast|multicast|unknown-unicast) level ([0-9.]+)$`)
	prioRe  = regexp.MustCompile(`^spanning-tree priority (\d+)$`)
	modeRe  = regexp.MustCompile(`^spanning-tree mode (\S+)$`)
)

// applyRunningConfig fills per-port configured state and switch-level STP
// from the running config.
type mirrorSession struct{ src, dst int }

var (
	mirrorSrcRe = regexp.MustCompile(`^monitor session (\d+) source (Ethernet[\d/]+)`)
	mirrorDstRe = regexp.MustCompile(`^monitor session (\d+) destination (Ethernet[\d/]+)`)
)

func applyRunningConfig(ports []switchmodel.Port, sys *switchmodel.System, rc showRunningConfig) {
	byIndex := map[int]*switchmodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	mirrorSessions := map[int]mirrorSession{}
	defer func() {
		for _, ms := range mirrorSessions {
			if p := byIndex[ms.dst]; p != nil && ms.src > 0 {
				p.MirrorFrom = ms.src
			}
		}
	}()
	for section, body := range rc.Cmds {
		if strings.HasPrefix(section, "interface Ethernet") {
			slot, lane, ok := parseEthName(strings.TrimPrefix(section, "interface "))
			if !ok || lane > 1 || body == nil {
				continue
			}
			p := byIndex[slot]
			if p == nil {
				continue
			}
			portfast, bpdufilter := false, false
			for line := range body.Cmds {
				switch {
				case line == "spanning-tree portfast":
					portfast = true
				case line == "spanning-tree bpdufilter enable":
					bpdufilter = true
				case line == "spanning-tree bpduguard enable":
					p.BPDUGuard = true
				case line == "error-correction encoding reed-solomon":
					p.FECConfig = switchmodel.FECRS
				case line == "error-correction encoding fire-code":
					p.FECConfig = switchmodel.FECFC
				case line == "no error-correction encoding":
					p.FECConfig = switchmodel.FECDisabled
				}
			}
			p.STPEdge = portfast && bpdufilter
			continue
		}
		if section == "ip dhcp snooping" {
			sys.DHCPSnooping = true
		}
		if strings.HasPrefix(section, "snmp-server community ") {
			sys.SNMPCommunities = append(sys.SNMPCommunities, strings.Fields(strings.TrimPrefix(section, "snmp-server community "))[0])
		}
		if strings.HasPrefix(section, "ntp server ") {
			sys.NTPServers = append(sys.NTPServers, strings.Fields(strings.TrimPrefix(section, "ntp server "))[0])
		}
		if strings.HasPrefix(section, "logging host ") {
			f := strings.Fields(strings.TrimPrefix(section, "logging host "))
			if len(f) >= 2 && f[1] != "514" {
				sys.SyslogHosts = append(sys.SyslogHosts, f[0]+":"+f[1])
			} else if len(f) >= 1 {
				sys.SyslogHosts = append(sys.SyslogHosts, f[0])
			}
		}
		if m := prioRe.FindStringSubmatch(section); m != nil {
			sys.STPPriority, _ = strconv.Atoi(m[1])
		}
		// monitor session <n> source EthernetX [both] / destination EthernetY
		if m := mirrorSrcRe.FindStringSubmatch(section); m != nil {
			n, _ := strconv.Atoi(m[1])
			slot, _, _ := parseEthName(m[2])
			mirrorSessions[n] = mirrorSession{src: slot, dst: mirrorSessions[n].dst}
		}
		if m := mirrorDstRe.FindStringSubmatch(section); m != nil {
			n, _ := strconv.Atoi(m[1])
			slot, _, _ := parseEthName(m[2])
			mirrorSessions[n] = mirrorSession{src: mirrorSessions[n].src, dst: slot}
		}
		if m := modeRe.FindStringSubmatch(section); m != nil && m[1] == "none" {
			sys.STPMode = "none"
		}
	}
}

func applyStormControl(ports []switchmodel.Port, sc showStormControl) {
	byIndex := map[int]*switchmodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for name, e := range sc.Interfaces {
		slot, _, ok := parseEthName(name)
		if !ok {
			continue
		}
		p := byIndex[slot]
		if p == nil || len(e.TrafficTypes) == 0 {
			continue
		}
		spec := &switchmodel.StormControlSpec{}
		for typ, t := range e.TrafficTypes {
			if t.ThresholdType != "percentage" {
				continue
			}
			pct := float64(t.Level) / 100
			switch typ {
			case "broadcast":
				spec.BroadcastPct = &pct
			case "multicast":
				spec.MulticastPct = &pct
			case "unknown-unicast", "unknownUnicast":
				spec.UnknownUnicastPct = &pct
			}
		}
		p.StormCtrl = spec
	}
}

func igmpSnooping(v showIGMPSnooping) map[int]bool {
	out := map[int]bool{}
	for k, e := range v.VLANs {
		if id, err := strconv.Atoi(k); err == nil {
			out[id] = e.State == "enabled" && v.State == "enabled"
		}
	}
	return out
}

// ---- apply side ----

func fecCommands(p switchmodel.Port, d switchmodel.PortDesired) []string {
	if d.FEC == nil || *d.FEC == p.FECConfig {
		return nil
	}
	switch *d.FEC {
	case switchmodel.FECRS:
		return []string{"error-correction encoding reed-solomon"}
	case switchmodel.FECFC:
		return []string{"error-correction encoding fire-code"}
	case switchmodel.FECDisabled:
		if p.FECConfig == "" {
			return nil // EOS default (auto-negotiated), nothing configured to remove
		}
		return []string{"no error-correction encoding"}
	}
	return nil
}

func pctEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func stormCommands(p switchmodel.Port, d switchmodel.PortDesired) []string {
	have, want := p.StormCtrl, d.StormCtrl
	if have == nil {
		have = &switchmodel.StormControlSpec{}
	}
	if want == nil {
		want = &switchmodel.StormControlSpec{}
	}
	var cmds []string
	for _, t := range []struct {
		kw   string
		have *float64
		want *float64
	}{
		{"broadcast", have.BroadcastPct, want.BroadcastPct},
		{"multicast", have.MulticastPct, want.MulticastPct},
		{"unknown-unicast", have.UnknownUnicastPct, want.UnknownUnicastPct},
	} {
		if pctEqual(t.have, t.want) {
			continue
		}
		if t.want == nil {
			cmds = append(cmds, "no storm-control "+t.kw)
		} else {
			cmds = append(cmds, fmt.Sprintf("storm-control %s level %s", t.kw, strconv.FormatFloat(*t.want, 'f', -1, 64)))
		}
	}
	return cmds
}

// stpEdgeCommands maps UniFi's per-port "STP disabled" onto an edge port
// that neither sends nor honours BPDUs: `spanning-tree portfast` +
// `spanning-tree bpdufilter enable` (verified on 4.26).
func stpEdgeCommands(p switchmodel.Port, d switchmodel.PortDesired) []string {
	switch {
	case d.STPEdge && !p.STPEdge:
		return []string{"spanning-tree portfast", "spanning-tree bpdufilter enable"}
	case !d.STPEdge && p.STPEdge:
		return []string{"no spanning-tree portfast", "no spanning-tree bpdufilter"}
	}
	return nil
}

func bpduGuardCommands(p switchmodel.Port, d switchmodel.PortDesired) []string {
	switch {
	case d.BPDUGuard && !p.BPDUGuard:
		return []string{"spanning-tree bpduguard enable"}
	case !d.BPDUGuard && p.BPDUGuard:
		return []string{"no spanning-tree bpduguard"}
	}
	return nil
}

// ApplySwitch implements switchmodel.SwitchController: STP mode/priority and
// per-VLAN IGMP snooping, diffed against the last snapshot.
func (c *Collector) ApplySwitch(ctx context.Context, d switchmodel.SwitchDesired) (int, error) {
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return 0, fmt.Errorf("eos apply switch: no snapshot yet")
	}
	var cmds []string
	if d.STPSet {
		mode := "none"
		if d.STPEnabled {
			switch d.STPMode {
			case "mstp":
				mode = "mstp"
			case "stp", "rstp", "":
				mode = "rstp" // EOS runs RSTP for both; classic STP is not offered
			default:
				mode = d.STPMode
			}
		}
		if mode != snap.System.STPMode {
			cmds = append(cmds, "spanning-tree mode "+mode)
		}
		if d.STPEnabled && d.STPPriority > 0 && d.STPPriority != snap.System.STPPriority {
			cmds = append(cmds, "spanning-tree priority "+strconv.Itoa(d.STPPriority))
		}
	}
	vlanIDs := make([]int, 0, len(d.IGMPSnooping))
	for id := range d.IGMPSnooping {
		vlanIDs = append(vlanIDs, id)
	}
	sort.Ints(vlanIDs)
	for _, id := range vlanIDs {
		want := d.IGMPSnooping[id]
		have, known := snap.System.IGMPSnooping[id]
		if known && have == want {
			continue
		}
		if want {
			cmds = append(cmds, "ip igmp snooping vlan "+strconv.Itoa(id))
		} else {
			cmds = append(cmds, "no ip igmp snooping vlan "+strconv.Itoa(id))
		}
	}
	// d.DHCPSnooping is deliberately ignored: UniFi's rogue DHCP server
	// detection cannot be honoured on EOS 4.26 (Option-82 insertion only,
	// no trusted-port model: `ip dhcp snooping trust` is invalid), so
	// `ip dhcp snooping` would sit "not operational" and block nothing
	// (verified live 2026-09-20). The capability is not claimed and the
	// loop logs the key as unsupported if a controller pushes it anyway.
	if d.SNMPSet {
		// UniFi manages one read-only community; other communities on the
		// switch are left alone, the previously managed one is replaced.
		want := []string{}
		if d.SNMPCommunity != "" {
			want = append(want, d.SNMPCommunity+" ro")
		}
		have := []string{}
		for _, cmty := range snap.System.SNMPCommunities {
			if cmty == d.SNMPCommunity || cmty == c.managedSNMP {
				have = append(have, cmty+" ro")
			}
		}
		cmds = append(cmds, listDiff("snmp-server community ", have, want)...)
	}
	if d.ManageNTP {
		cmds = append(cmds, listDiff("ntp server ", snap.System.NTPServers, d.NTPServers)...)
	}
	if d.ManageSyslog {
		cmds = append(cmds, listDiff("logging host ", snap.System.SyslogHosts, d.SyslogHosts)...)
	}
	if len(cmds) == 0 {
		return 0, nil
	}
	batch := append([]string{"enable", "configure"}, cmds...)
	batch = append(batch, "end", "write memory")
	if err := c.t.Configure(ctx, batch); err != nil {
		return 0, fmt.Errorf("eos apply switch settings: %w", err)
	}
	c.mu.Lock()
	if c.last == snap {
		updated := *snap
		if d.STPSet {
			if d.STPEnabled {
				updated.System.STPMode = "rstp"
				if d.STPPriority > 0 {
					updated.System.STPPriority = d.STPPriority
				}
			} else {
				updated.System.STPMode = "none"
			}
		}
		updated.System.IGMPSnooping = map[int]bool{}
		for k, v := range snap.System.IGMPSnooping {
			updated.System.IGMPSnooping[k] = v
		}
		for k, v := range d.IGMPSnooping {
			updated.System.IGMPSnooping[k] = v
		}
		if d.SNMPSet {
			c.managedSNMP = d.SNMPCommunity
			var keep []string
			for _, cmty := range snap.System.SNMPCommunities {
				if cmty != d.SNMPCommunity {
					keep = append(keep, cmty)
				}
			}
			if d.SNMPCommunity != "" {
				keep = append(keep, d.SNMPCommunity)
			}
			updated.System.SNMPCommunities = keep
		}
		if d.ManageNTP {
			updated.System.NTPServers = append([]string(nil), d.NTPServers...)
		}
		if d.ManageSyslog {
			updated.System.SyslogHosts = append([]string(nil), d.SyslogHosts...)
		}
		c.last = &updated
	}
	c.mu.Unlock()
	return len(cmds), nil
}

// listDiff turns a have/want list of "<prefix><value>" config lines into
// the add/remove commands. Syslog hosts are "host[:port]" and become
// `logging host <host> [port]`.
func listDiff(prefix string, have, want []string) []string {
	norm := func(v string) string {
		if prefix == "logging host " {
			h, port, ok := strings.Cut(v, ":")
			if ok {
				return h + " " + port
			}
		}
		return v
	}
	haveSet := map[string]bool{}
	for _, v := range have {
		haveSet[norm(v)] = true
	}
	wantSet := map[string]bool{}
	for _, v := range want {
		wantSet[norm(v)] = true
	}
	var cmds []string
	for _, v := range want {
		if !haveSet[norm(v)] {
			cmds = append(cmds, prefix+norm(v))
		}
	}
	for _, v := range have {
		if !wantSet[norm(v)] {
			cmds = append(cmds, "no "+prefix+strings.TrimSuffix(norm(v), " ro"))
		}
	}
	return cmds
}

// CyclePort implements switchmodel.PortCycler: shutdown every lane, wait,
// no shutdown. The controller's "port-cycle" command.
func (c *Collector) CyclePort(ctx context.Context, idx int) error {
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return fmt.Errorf("eos port-cycle: no snapshot yet")
	}
	var lanes []string
	for _, p := range snap.Ports {
		if p.Index == idx {
			lanes = p.Interfaces
			if len(lanes) == 0 {
				lanes = []string{p.IfName}
			}
		}
	}
	if len(lanes) == 0 {
		return fmt.Errorf("eos port-cycle: no port %d", idx)
	}
	down := []string{"enable", "configure"}
	up := []string{"enable", "configure"}
	for _, l := range lanes {
		down = append(down, "interface "+l, "shutdown")
		up = append(up, "interface "+l, "no shutdown")
	}
	if err := c.t.Configure(ctx, append(down, "end")); err != nil {
		return fmt.Errorf("eos port-cycle down: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
	}
	if err := c.t.Configure(ctx, append(up, "end")); err != nil {
		return fmt.Errorf("eos port-cycle up: %w", err)
	}
	return nil
}

// Reboot implements switchmodel.Rebooter: save and reload. EOS asks for
// confirmation on `reload`; `reload now` skips it.
func (c *Collector) Reboot(ctx context.Context) error {
	return c.t.Configure(ctx, []string{"enable", "write memory", "reload now"})
}

// InstallSSHKeys implements switchmodel.SSHKeyInstaller for the bridge's own
// user (the one the transport authenticates as, or SSHKeyUser when set): EOS
// keeps one primary and one secondary key per user, so the first two
// enabled controller keys are installed and the rest reported as skipped.
func (c *Collector) InstallSSHKeys(ctx context.Context, keys []switchmodel.SSHKey) (int, error) {
	user := c.sshKeyUser
	if user == "" {
		return 0, fmt.Errorf("eos ssh keys: no target user configured")
	}
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return 0, fmt.Errorf("eos ssh keys: no snapshot yet")
	}
	want := make([]string, 0, 2)
	for _, k := range keys {
		line := strings.TrimSpace(k.Type + " " + k.Value + " " + strings.ReplaceAll(k.Comment, " ", "_"))
		want = append(want, line)
		if len(want) == 2 {
			break
		}
	}
	have := c.installedKeys
	if len(want) == 0 || keyValuesEqual(have, want) {
		return 0, nil
	}
	cmds := []string{"enable", "configure"}
	if len(want) >= 1 {
		cmds = append(cmds, "username "+user+" ssh-key "+want[0])
	}
	if len(want) >= 2 {
		cmds = append(cmds, "username "+user+" ssh-key secondary "+want[1])
	}
	cmds = append(cmds, "end", "write memory")
	if err := c.t.Configure(ctx, cmds); err != nil {
		return 0, fmt.Errorf("eos ssh keys: %w", err)
	}
	c.mu.Lock()
	c.installedKeys = want
	c.mu.Unlock()
	return len(want), nil
}

// keyValuesEqual compares key lines by "<type> <value>" only: EOS may
// store or echo the comment differently, and a key present on the switch
// must not be rewritten every start.
func keyValuesEqual(have, want []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		f := strings.Fields(h)
		if len(f) >= 2 {
			set[f[0]+" "+f[1]] = true
		}
	}
	for _, w := range want {
		f := strings.Fields(w)
		if len(f) < 2 || !set[f[0]+" "+f[1]] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sshKeysFromRunningConfig reads the user's primary/secondary ssh-key lines
// as "<type> <value> <comment>" so InstallSSHKeys can diff against them.
func sshKeysFromRunningConfig(rc showRunningConfig, user string) []string {
	var primary, secondary string
	for section := range rc.Cmds {
		if !strings.HasPrefix(section, "username "+user+" ssh-key ") {
			continue
		}
		rest := strings.TrimPrefix(section, "username "+user+" ssh-key ")
		if strings.HasPrefix(rest, "secondary ") {
			secondary = strings.TrimPrefix(rest, "secondary ")
		} else {
			primary = rest
		}
	}
	out := []string{}
	if primary != "" {
		out = append(out, primary)
	}
	if secondary != "" {
		out = append(out, secondary)
	}
	return out
}

// showErrdisabled is `show interfaces status errdisabled`.
type showErrdisabled struct {
	InterfaceStatuses map[string]struct {
		CauseReasons []string `json:"causes"`
		Reason       string   `json:"reason"`
	} `json:"interfaceStatuses"`
}

func applyErrdisabled(ports []switchmodel.Port, ed showErrdisabled) {
	byIndex := map[int]*switchmodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for name, e := range ed.InterfaceStatuses {
		slot, _, ok := parseEthName(name)
		if !ok {
			continue
		}
		p := byIndex[slot]
		if p == nil {
			continue
		}
		reason := e.Reason
		if reason == "" && len(e.CauseReasons) > 0 {
			reason = strings.Join(e.CauseReasons, ",")
		}
		if reason == "" {
			reason = "errdisabled"
		}
		if p.Fault == "" {
			p.Fault = "errdisabled: " + reason
		}
	}
}

// lagCommands: join or leave a channel group. Every lane of a cage joins
// the same group (LACP active). The Port-Channel interface gets the VLAN
// config in vlanTarget.
func lagCommands(p switchmodel.Port, d switchmodel.PortDesired) []string {
	switch {
	case d.LAG > 0 && p.LAGID != d.LAG:
		return []string{"channel-group " + strconv.Itoa(d.LAG) + " mode active"}
	case d.LAG == 0 && p.LAGID > 0:
		return []string{"no channel-group"}
	}
	return nil
}

// mirrorCommands are switch-level: session number = destination port index.
func mirrorCommands(ports []switchmodel.Port, desired []switchmodel.PortDesired) []string {
	byIndex := map[int]switchmodel.Port{}
	for _, p := range ports {
		byIndex[p.Index] = p
	}
	var cmds []string
	for _, d := range desired {
		p, ok := byIndex[d.Index]
		if !ok {
			continue
		}
		switch {
		case d.MirrorSource > 0 && p.MirrorFrom != d.MirrorSource:
			src, ok := byIndex[d.MirrorSource]
			if !ok {
				continue
			}
			n := strconv.Itoa(d.Index)
			if p.MirrorFrom > 0 {
				cmds = append(cmds, "no monitor session "+n) // EOS refuses to remove a session that does not exist
			}
			for _, lane := range laneNames(src) {
				cmds = append(cmds, "monitor session "+n+" source "+lane+" both")
			}
			cmds = append(cmds, "monitor session "+n+" destination "+laneNames(p)[0])
		case d.MirrorSource == 0 && p.MirrorFrom > 0:
			cmds = append(cmds, "no monitor session "+strconv.Itoa(d.Index))
		}
	}
	return cmds
}

func laneNames(p switchmodel.Port) []string {
	if len(p.Interfaces) > 0 {
		return p.Interfaces
	}
	return []string{p.IfName}
}
