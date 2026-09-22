// Package device is the device-side inform state machine for one emulated
// UniFi switch, plus the payload it reports.
//
// The state machine is forked from github.com/jamesbraid/unifi-emu/inform
// (session.go, tables.go; MIT, Copyright (c) James Braid) and differs in
// three ways: adoption state persists to disk so a restart does not lose the
// controller's key; the switch tables come from a live devicemodel.Snapshot
// instead of constants; and controller-pushed port config is merged over the
// live table rather than replacing it. The wire format and crypto are used
// unchanged from unifi-emu's inform package.
package device

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// Session holds the mutable adoption state and renders the inform payload.
// Safe for concurrent use: the inform loop and the collector both touch it.
type Session struct {
	mu sync.Mutex

	desc      inform.Descriptor
	macHeader [6]byte
	st        State
	store     *Store // nil = no persistence
	snap      *devicemodel.Snapshot
	bootTime  time.Time // fallback uptime clock when no snapshot
	locating  bool

	versionPinned bool                // the operator named a version: never follow the switch's
	prevHistory   map[int]portHistory // per port, at the last inform (anomaly deltas)
	caps          devicemodel.Capabilities
	gatewayIP     string // reported as gateway_ip; "" = omit
}

// SetUplinkPort marks idx as the uplink in the reported port table (0 = no
// change): the loop re-evaluates the uplink from LLDP on every collect,
// because the neighbour view at startup can be incomplete.
func (s *Session) SetUplinkPort(idx int) {
	if idx <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.desc.Ports {
		s.desc.Ports[i].IsUplink = s.desc.Ports[i].PortIdx == idx
	}
}

// SetCapabilities replaces the capability claims (default: DefaultCapabilities).
func (s *Session) SetCapabilities(c devicemodel.Capabilities) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps = c
}

// NewSession starts a device from st (a fresh State means factory-default:
// default key, pending, CBC). informURL is used unless st already carries one
// from a set-adopt. store may be nil.
func NewSession(desc inform.Descriptor, informURL string, st State, store *Store, now time.Time) *Session {
	if st.Key == "" {
		st.Key = inform.DefaultKey
	}
	if st.CfgVersion == "" {
		st.CfgVersion = "0"
	}
	if st.InformURL == "" {
		st.InformURL = informURL
	}
	if st.Firmware != "" {
		// A previous emulated upgrade is still reported — unless the switch
		// itself has been upgraded since, in which case the truth wins.
		if st.FirmwareBase == "" || st.FirmwareBase == desc.Version {
			desc.Version = st.Firmware
		} else {
			st.Firmware, st.FirmwareBase = "", ""
		}
	}
	s := &Session{desc: desc, st: st, store: store, bootTime: now, caps: DefaultCapabilities}
	s.macHeader = macHeader(desc.MAC)
	return s
}

func (s *Session) AuthKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Key
}

func (s *Session) InformURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.InformURL
}

func (s *Session) Adopted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Adopted
}

func (s *Session) UseAESGCM() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.UseAESGCM
}

// Pending returns the config push waiting to be applied, if any.
func (s *Session) Pending() (cfgversion, systemCfg string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.PendingCfgVersion, s.st.PendingSystemCfg, s.st.PendingSystemCfg != ""
}

// Version is the firmware version currently reported.
func (s *Session) Version() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desc.Version
}

// Applied returns the last system_cfg applied to the switch, if any.
func (s *Session) Applied() (cfgversion, systemCfg string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.CfgVersion, s.st.SystemCfg, s.st.SystemCfg != ""
}

// MarkApplied records that the pending push is live on the switch: the
// device starts reporting its cfgversion and the controller stops re-sending.
func (s *Session) MarkApplied(cfgversion string) error {
	s.mu.Lock()
	if s.st.PendingCfgVersion != cfgversion {
		s.mu.Unlock()
		return nil // a newer push superseded it; leave that one pending
	}
	s.st.CfgVersion = cfgversion
	s.st.SystemCfg = s.st.PendingSystemCfg
	s.st.PendingCfgVersion, s.st.PendingSystemCfg = "", ""
	st := s.st.clone()
	s.mu.Unlock()
	if s.store != nil {
		return s.store.Save(st)
	}
	return nil
}

// Snapshot returns the current switch snapshot (nil before the first poll).
func (s *Session) Snapshot() *devicemodel.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// SetSnapshot replaces the switch state the next payload reports.
func (s *Session) SetSnapshot(snap *devicemodel.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = snap
	// Report the switch's own firmware version, and follow it when the
	// switch is upgraded under a running bridge. An operator's -version
	// wins; so does an emulated upgrade, until the switch's version moves.
	v := ""
	if snap != nil {
		v = snap.System.Version
	}
	if v == "" || s.versionPinned {
		return
	}
	if s.st.Firmware != "" {
		if s.st.FirmwareBase == "" || s.st.FirmwareBase == v {
			return // the emulated upgrade still stands
		}
		s.st.Firmware, s.st.FirmwareBase = "", "" // really upgraded: drop the fiction
	}
	s.desc.Version = v
}

// PinVersion freezes the reported firmware version: the operator named one
// (-version / version:), so neither the switch nor an emulated upgrade
// changes it.
func (s *Session) PinVersion() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versionPinned = true
}

// deviceVersion is the switch's own version at the last collect.
func (s *Session) deviceVersion() string {
	if s.snap == nil {
		return ""
	}
	return s.snap.System.Version
}

// EncodeInform builds the current payload and encrypts it in the negotiated
// mode (AES-GCM once the controller enabled it, AES-CBC before).
func (s *Session) EncodeInform(now time.Time) ([]byte, error) {
	s.mu.Lock()
	payload := s.buildPayload(now)
	key, gcm := s.st.Key, s.st.UseAESGCM
	s.mu.Unlock()
	pkt := &inform.Packet{MAC: s.macHeader, Payload: payload}
	if gcm {
		return pkt.EncodeGCM(key)
	}
	return pkt.Encode(key)
}

// BuildPayload renders the payload for the current state (for tests/debug).
func (s *Session) BuildPayload(now time.Time) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buildPayload(now)
}

func (s *Session) buildPayload(now time.Time) []byte {
	uptime := int64(now.Sub(s.bootTime).Seconds())
	if s.snap != nil && s.snap.System.Uptime > 0 {
		uptime = int64(s.snap.System.Uptime.Seconds())
	}
	m := map[string]any{
		"mac":            s.desc.MAC,
		"serial":         s.desc.Serial,
		"model":          s.desc.Model,
		"model_display":  s.desc.ModelDisplay,
		"version":        s.desc.Version,
		"ip":             s.desc.IP,
		"hostname":       s.desc.Hostname,
		"inform_url":     s.st.InformURL,
		"uptime":         uptime,
		"time":           now.Unix(),
		"cfgversion":     s.st.CfgVersion,
		"x_authkey":      s.st.Key,
		"default":        !s.st.Adopted,
		"_default_key":   !s.st.Adopted,
		"state":          1,
		"fw_caps":        s.desc.FWCaps,
		"isolated":       false,
		"locating":       s.locating,
		"selfrun_beacon": true,
	}
	// UDAPI config-plane fields go out together or not at all (see
	// docs/feature-map.md): a bare udapi_caps drops the whole capability
	// update on the controller.
	if s.desc.UDAPIVersion != "" {
		m["udapi_version"] = map[string]any{"version": s.desc.UDAPIVersion}
		m["udapi_caps"] = s.desc.UDAPICaps
	}
	if s.st.Adopted {
		// Device-side state 4 = managed. Not the REST stat/device enum.
		m["state"] = 4
		m["bootrom_version"] = "unknown"
		// Identity fields every UniFi switch reports (values are what this
		// controller's own switches send; none of them is a capability).
		m["manufacturer_id"] = 61
		m["required_version"] = "0.1.7"
		m["architecture"] = "x86_64"
		m["kernel_version"] = "4.19.0-12-2-amd64"
		m["board_rev"] = 6
		m["inform_min_interval"] = 30
		m["stats_inform_interval"] = 24
		m["provisioning_timeout"] = 300
		m["reboot_duration"] = 240
		m["upgrade_duration"] = 300
		m["anon_id"] = anonID(s.desc.MAC)
		m["guid"] = anonID("guid " + s.desc.MAC)
		m["hash_id"] = anonID("hash " + s.desc.MAC)[:16]
		m["boot"] = map[string]any{"id": anonID("boot " + s.desc.MAC + s.bootTime.String())}
		m["bootid"] = -1
		m["dualboot"] = false
		m["fan_emergency"] = 0
		m["time_ms"] = now.Nanosecond() / 1e6
		m["has_eth1"] = false
		m["discovery_response"] = false
		m["ssh_session_table"] = []any{}
		m["network_table"] = []any{}
		m["dhcp_server_table"] = []any{}
		m["last_error_conns"] = []any{}
		m["ever_crash"] = false
		m["internet"] = true
		m["tm_ready"] = true
		m["default"] = false
		m["time"] = now.Unix()
		m["timestamp"] = now.UTC().Format("2006-01-02T15:04:05")
		m["uptime_str"] = uptimeStr(uptime)
		m["satisfaction_reason"] = 0
		if s.gatewayIP != "" {
			m["gateway_ip"] = s.gatewayIP
		}
		m["sys_stats"] = sysStats(s.snap)
		m["system-stats"] = systemStats(s.snap, uptime)
		if mtc := macTableCapability(s.snap); mtc != nil {
			m["mac_table_capability"] = mtc
		}
		if s.snap != nil && s.snap.System.HasTemperature {
			m["general_temperature"] = int(s.snap.System.TemperatureC + 0.5)
			m["has_temperature"] = true
		}
		m["switch_caps"] = switchCaps(s.caps)
		pt := portTable(s.desc, s.snap, s.st.Provisioned["port_table"], s.prevHistory)
		m["port_table"] = pt
		// Device-level satisfaction (the Experience column): the mean of the
		// live ports' scores, as UniFi switches report it top-level.
		if sum, n := 0, 0; true {
			for _, e := range pt {
				if v, ok := e["satisfaction"].(int); ok {
					sum, n = sum+v, n+1
				}
			}
			if n > 0 {
				m["satisfaction"] = sum / n
			}
		}
		if s.snap != nil {
			s.prevHistory = make(map[int]portHistory, len(s.snap.Ports))
			for _, p := range s.snap.Ports {
				s.prevHistory[p.Index] = portHistory{At: s.snap.TakenAt, Counters: p.Counters, LinkChanges: p.Health.LinkChanges,
					STPChanges: p.Health.STPChanges, FECUncorrected: p.Health.FECUncorrected, PCSErrBlocks: p.Health.PCSErrBlocks}
			}
		}
		// Reachability, as UniFi switches report it: where the controller can
		// connect back to the device, its netmask and its gateway's MAC. The
		// controller uses these to place the device in a network.
		m["connect_request_ip"] = s.desc.IP
		m["connect_request_port"] = "22"
		if nm := netmaskFor(s.snap, s.desc.IP); nm != "" {
			m["netmask"] = nm
		}
		if s.snap != nil {
			gw := s.snap.System.GatewayMAC
			if gw == "" && s.gatewayIP != "" {
				gw = s.snap.System.ARP[s.gatewayIP]
			}
			if gw != "" {
				m["gateway_mac"] = gw
			}
		}
		if mac := serviceMAC(s.snap, s.desc.MAC); mac != "" {
			m["service_mac"] = mac
		}
		if lt := lldpTable(s.desc, s.snap); len(lt) > 0 {
			m["lldp_table"] = lt
		}
		for k, v := range deviceTables(s.desc, s.snap) {
			m[k] = v
		}
	}
	// Echo provisioned config the controller pushed via setstate, except
	// port_table, which is merged into the live table above.
	for k, v := range s.st.Provisioned {
		if k != "port_table" {
			m[k] = v
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil // unreachable: only JSON-safe values above
	}
	return b
}

// informResponse is the controller's reply. mgmt_cfg is newline-separated
// k=v text, not JSON.
type informResponse struct {
	Type       string `json:"_type"`
	Cmd        string `json:"cmd"`
	Key        string `json:"key"`
	URI        string `json:"uri"`
	Interval   int    `json:"interval"`
	MgmtCfg    string `json:"mgmt_cfg"`
	SystemCfg  string `json:"system_cfg"`
	Cfgversion string `json:"cfgversion"`
	Version    string `json:"version"`
	PortIdx    int    `json:"port_idx"`
}

// Our own Effect kinds, outside unifi-emu's range.
const (
	// EffectSystemCfg: the reply carried a system_cfg (Text = its cfgversion)
	// that now waits in State.Pending* for the controller side to apply.
	EffectSystemCfg inform.EffectKind = 100 + iota
	// EffectLocate: the controller asked the LEDs to blink (Text = "on"/"off");
	// the payload reports `locating` accordingly.
	EffectLocate
	// EffectPortCycle: bounce a port (Interval unused; Text = port_idx).
	EffectPortCycle
)

// Apply advances the session by one controller reply and returns what
// changed, using unifi-emu's Effect vocabulary. State is persisted when it
// changed. The key-rotation rule: a mgmt_cfg.authkey is adopted only while
// the device still holds the default key; set-adopt rotates unconditionally.
func (s *Session) Apply(now time.Time, body []byte) []inform.Effect {
	var r informResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return []inform.Effect{{Kind: inform.EffectDecodeError, Text: err.Error()}}
	}
	s.mu.Lock()
	before := s.st.clone()
	var effects []inform.Effect
	switch r.Type {
	case "cmd":
		effects = s.applyCmd(now, r)
	case "setparam":
		effects = s.applySetparam(r)
	case "setstate":
		effects = s.applySetstate(body, r.Cfgversion)
	case "noop":
		if r.Interval > 0 {
			effects = []inform.Effect{{Kind: inform.EffectInterval, Interval: time.Duration(r.Interval) * time.Second}}
		}
	case "upgrade", "upgrade2":
		// Emulated: accept the target version, "reboot", and report it from
		// now on. The controller then sees an up-to-date device and stops
		// offering the upgrade. Persisted via State.Firmware.
		if r.Version != "" {
			s.desc.Version = r.Version
			s.st.Firmware, s.st.FirmwareBase = r.Version, s.deviceVersion()
		}
		s.bootTime = now
		effects = []inform.Effect{{Kind: inform.EffectUpgraded, Text: r.Version}}
	default:
		effects = []inform.Effect{{Kind: inform.EffectUnknownType, Text: r.Type}}
	}
	changed := !s.st.equal(before)
	st := s.st.clone()
	s.mu.Unlock()

	if changed && s.store != nil {
		if err := s.store.Save(st); err != nil {
			effects = append(effects, inform.Effect{Kind: inform.EffectDecodeError, Text: "persist state: " + err.Error()})
		}
	}
	return effects
}

func (s *Session) applyCmd(now time.Time, r informResponse) []inform.Effect {
	switch r.Cmd {
	case "set-adopt", "adopt":
		if r.Key != "" {
			s.st.Key = r.Key
		}
		if r.URI != "" {
			s.st.InformURL = r.URI
		}
		s.st.Adopted = true
		return []inform.Effect{{Kind: inform.EffectAdoptingViaSetAdopt, Text: r.URI}}
	case "setdefault":
		s.st.Adopted = false
		s.st.Key = inform.DefaultKey
		s.st.CfgVersion = "0"
		s.st.UseAESGCM = false
		s.st.Provisioned = nil
		return []inform.Effect{{Kind: inform.EffectFactoryReset}}
	case "reboot":
		s.bootTime = now
		return []inform.Effect{{Kind: inform.EffectRebooted}}
	case "locate", "set-locate": // "set-locate" is what Network 10.6 sends (captured 2026-09-20)
		s.locating = true
		return []inform.Effect{{Kind: EffectLocate, Text: "on"}}
	case "unlocate", "unset-locate":
		s.locating = false
		return []inform.Effect{{Kind: EffectLocate, Text: "off"}}
	case "power-cycle", "port-cycle", "port_cycle":
		// The controller's port power cycle. Network 10.6 only issues it for a
		// PoE port that is powering a device (cmd/devmgr power-cycle answers
		// api.err.InvalidTargetPort for any other port, and the UI offers
		// "Power Cycle" only there), so a switch without PoE never receives it
		// and the device-side name has not been captured; "power-cycle" is the
		// API's name, the others are kept for older spellings.
		return []inform.Effect{{Kind: EffectPortCycle, Text: strconv.Itoa(r.PortIdx)}}
	case "upgrade", "upgrade2":
		if r.Version != "" {
			s.desc.Version = r.Version
			s.st.Firmware, s.st.FirmwareBase = r.Version, s.deviceVersion()
		}
		s.bootTime = now
		return []inform.Effect{{Kind: inform.EffectUpgraded, Text: r.Version}}
	default:
		return []inform.Effect{{Kind: inform.EffectUnknownCmd, Text: r.Cmd}}
	}
}

func (s *Session) applySetparam(r informResponse) []inform.Effect {
	effects := []inform.Effect{{Kind: inform.EffectMgmtCfg, Text: r.MgmtCfg}}
	var cfgvers, authkey, useAESGCM string
	for _, line := range strings.Split(r.MgmtCfg, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "cfgversion":
			cfgvers = v
		case "authkey":
			authkey = v
		case "use_aes_gcm":
			useAESGCM = v
		}
	}
	switch {
	case r.SystemCfg != "":
		// A config push. Do not claim its cfgversion until it is applied.
		v := r.Cfgversion
		if v == "" {
			v = cfgvers
		}
		s.st.PendingCfgVersion = v
		s.st.PendingSystemCfg = r.SystemCfg
		effects = append(effects, inform.Effect{Kind: EffectSystemCfg, Text: v})
	case cfgvers != "":
		s.st.CfgVersion = cfgvers
	}
	if authkey != "" && authkey != inform.DefaultKey && s.st.Key == inform.DefaultKey {
		s.st.Key = authkey
		s.st.Adopted = true
		effects = append(effects, inform.Effect{Kind: inform.EffectAdoptingViaMgmtCfg})
	}
	if useAESGCM != "" {
		if enabled, err := strconv.ParseBool(useAESGCM); err == nil {
			s.st.UseAESGCM = enabled
		}
	}
	return effects
}

func (s *Session) applySetstate(body []byte, cfgversion string) []inform.Effect {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return []inform.Effect{{Kind: inform.EffectDecodeError, Text: err.Error()}}
	}
	if cfgversion != "" {
		s.st.CfgVersion = cfgversion
	}
	if s.st.Provisioned == nil {
		s.st.Provisioned = map[string]json.RawMessage{}
	}
	var keys []string
	for k, v := range raw {
		if strings.HasPrefix(k, "_") || k == "cfgversion" {
			continue
		}
		s.st.Provisioned[k] = v
		keys = append(keys, k)
	}
	// Reported as an "unknown cmd"-style effect so the loop logs it: every
	// setstate is potential phase 1 input and must be visible.
	return []inform.Effect{{Kind: inform.EffectUnknownCmd, Text: "setstate keys: " + strings.Join(keys, ",")}}
}

// anonID is the device's stable anonymous id, derived from its MAC (a real
// device generates and keeps one; ours must not change between restarts).
//
// The salt still carries the project's old name. It is not a label: it is the
// input to an id already reported to the controller for every adopted device,
// and changing the string changes that id. It stays as it is.
func anonID(mac string) string {
	h := sha1.Sum([]byte("switch-to-unifi anon " + strings.ToLower(mac)))
	h[6] = (h[6] & 0x0f) | 0x50 // version 5 shape
	h[8] = (h[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// SetGatewayIP sets the gateway/controller address reported as gateway_ip.
func (s *Session) SetGatewayIP(ip string) { s.mu.Lock(); s.gatewayIP = ip; s.mu.Unlock() }

// uptimeStr renders seconds the way UniFi devices do ("21h25m23s").
func uptimeStr(secs int64) string {
	d := secs / 86400
	h := (secs % 86400) / 3600
	mi := (secs % 3600) / 60
	sec := secs % 60
	if d > 0 {
		return fmt.Sprintf("%dd%dh%dm%ds", d, h, mi, sec)
	}
	return fmt.Sprintf("%dh%dm%ds", h, mi, sec)
}
