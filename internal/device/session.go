// Package device is the device-side inform state machine for one emulated
// UniFi switch, plus the payload it reports.
//
// The state machine is forked from github.com/jamesbraid/unifi-emu/inform
// (session.go, tables.go; MIT, Copyright (c) James Braid) and differs in
// three ways: adoption state persists to disk so a restart does not lose the
// controller's key; the switch tables come from a live switchmodel.Snapshot
// instead of constants; and controller-pushed port config is merged over the
// live table rather than replacing it. The wire format and crypto are used
// unchanged from unifi-emu's inform package.
package device

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
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
	snap      *switchmodel.Snapshot
	bootTime  time.Time // fallback uptime clock when no snapshot
	locating  bool

	prevHistory map[int]portHistory // per port, at the last inform (anomaly deltas)
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
		desc.Version = st.Firmware // a previous emulated upgrade wins over the profile default
	}
	s := &Session{desc: desc, st: st, store: store, bootTime: now}
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
func (s *Session) Snapshot() *switchmodel.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// SetSnapshot replaces the switch state the next payload reports.
func (s *Session) SetSnapshot(snap *switchmodel.Snapshot) {
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
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
		m["sys_stats"] = sysStats(s.snap)
		m["system-stats"] = systemStats(s.snap, uptime)
		if mtc := macTableCapability(s.snap); mtc != nil {
			m["mac_table_capability"] = mtc
		}
		if s.snap != nil && s.snap.System.HasTemperature {
			m["general_temperature"] = int(s.snap.System.TemperatureC + 0.5)
			m["has_temperature"] = true
		}
		m["switch_caps"] = switchCaps()
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
				s.prevHistory[p.Index] = portHistory{Counters: p.Counters, LinkChanges: p.Health.LinkChanges,
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
		if s.snap != nil && s.snap.System.GatewayMAC != "" {
			m["gateway_mac"] = s.snap.System.GatewayMAC
		}
		m["ethernet_table"] = ethernetTable(s.desc, s.snap)
		if mac := serviceMAC(s.snap, s.desc.MAC); mac != "" {
			m["service_mac"] = mac
		}
		if lt := lldpTable(s.desc, s.snap); len(lt) > 0 {
			m["lldp_table"] = lt
		}
		for k, v := range switchTables(s.desc, s.snap) {
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
			s.st.Firmware = r.Version
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
	case "locate":
		s.locating = true
		return []inform.Effect{{Kind: EffectLocate, Text: "on"}}
	case "unlocate":
		s.locating = false
		return []inform.Effect{{Kind: EffectLocate, Text: "off"}}
	case "port-cycle", "port_cycle":
		return []inform.Effect{{Kind: EffectPortCycle, Text: strconv.Itoa(r.PortIdx)}}
	case "upgrade", "upgrade2":
		if r.Version != "" {
			s.desc.Version = r.Version
			s.st.Firmware = r.Version
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
