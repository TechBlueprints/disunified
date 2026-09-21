package device

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/switchmodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

func testDesc() inform.Descriptor {
	ports := make([]inform.Port, 0, 54)
	for i := 1; i <= 54; i++ {
		media := "SFP28"
		if i > 48 {
			media = "QSFP28"
		}
		ports = append(ports, inform.Port{IfName: "eth" + itoa(i-1), Name: "Port " + itoa(i), PortIdx: i, Media: media, IsUplink: i == 49})
	}
	return inform.Descriptor{
		MAC: "02:00:00:00:00:3c", Serial: "X", Model: "UDC48X6", ModelDisplay: "UniFi Data Center 100G-48X6",
		Version: "7.3.109.16640", IP: "192.0.2.9", Hostname: "arista", Type: "usw", FWCaps: inform.PlaceholderFWCaps, Ports: ports,
	}
}

func itoa(i int) string { return string(rune('0'+i/10)) + string(rune('0'+i%10)) }

func testSnapshot() *switchmodel.Snapshot {
	snap := &switchmodel.Snapshot{TakenAt: time.Now()}
	snap.System = switchmodel.System{Uptime: 1000 * time.Second, CPUPercent: 12.5, MemTotalKB: 100, MemUsedKB: 40, MemBufferKB: 10, TemperatureC: 63.4, HasTemperature: true}
	for i := 1; i <= 54; i++ {
		p := switchmodel.Port{Index: i, IfName: "Ethernet" + itoa(i), Name: "Ethernet" + itoa(i), Media: switchmodel.MediaCopper10G, Lanes: 1, Present: true, Enabled: true, MTU: 9214}
		if i > 48 {
			p.Media = switchmodel.MediaQSFP28
		}
		if i == 49 {
			p.Up, p.SpeedMbps, p.FullDuplex, p.STPState = true, 100000, true, "forwarding"
			p.Counters = switchmodel.Counters{RxBytes: 111, TxBytes: 222, RxPackets: 3, TxPackets: 4, RxErrors: 5}
			p.Neighbor = &switchmodel.Neighbor{SystemName: "agg", ChassisID: "aa:bb:cc:dd:ee:ff", PortID: "one00GigE48"}
		}
		if i == 52 {
			p.Present = false
		}
		snap.Ports = append(snap.Ports, p)
	}
	return snap
}

func payload(t *testing.T, s *Session) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.BuildPayload(time.Unix(1_800_000_000, 0)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPendingPayloadIsSparse(t *testing.T) {
	s := NewSession(testDesc(), "http://192.0.2.1:8080/inform", State{}, nil, time.Now())
	m := payload(t, s)
	if m["state"] != float64(1) || m["default"] != true || m["x_authkey"] != inform.DefaultKey {
		t.Errorf("pending payload = %v", m)
	}
	if _, ok := m["port_table"]; ok {
		t.Error("pending payload must not carry port_table")
	}
	if s.UseAESGCM() || s.Adopted() {
		t.Error("fresh session must be CBC and unadopted")
	}
}

func TestAdoptedPayloadUsesSnapshot(t *testing.T) {
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	s := NewSession(testDesc(), "http://192.0.2.1:8080/inform", st, nil, time.Now())
	s.SetSnapshot(testSnapshot())
	m := payload(t, s)
	if m["state"] != float64(4) || m["uptime"] != float64(1000) || m["general_temperature"] != float64(63) {
		t.Errorf("adopted payload header = state %v uptime %v temp %v", m["state"], m["uptime"], m["general_temperature"])
	}
	ss := m["sys_stats"].(map[string]any)
	if ss["cpu"] != 12.5 || ss["mem_total"] != float64(100*1024) {
		t.Errorf("sys_stats = %v", ss)
	}
	pt := m["port_table"].([]any)
	if len(pt) != 54 {
		t.Fatalf("port_table has %d entries", len(pt))
	}
	p49 := pt[48].(map[string]any)
	if p49["up"] != true || p49["speed"] != float64(100000) || p49["rx_bytes"] != float64(111) || p49["rx_errors"] != float64(5) || p49["stp_state"] != "forwarding" || p49["is_uplink"] != true || p49["sfp_found"] != true {
		t.Errorf("port 49 = %v", p49)
	}
	p52 := pt[51].(map[string]any)
	if p52["sfp_found"] != false || p52["up"] != false {
		t.Errorf("port 52 = %v", p52)
	}
	p1 := pt[0].(map[string]any)
	if _, ok := p1["sfp_found"]; ok {
		t.Errorf("copper port 1 must not report sfp_found: %v", p1)
	}
	lt := m["lldp_table"].([]any)
	if len(lt) != 1 || lt[0].(map[string]any)["chassis_id"] != "aa:bb:cc:dd:ee:ff" || lt[0].(map[string]any)["local_port_name"] != "Ethernet49" { // the vendor name, matching our LLDP port ID
		t.Errorf("lldp_table = %v", lt)
	}
}

func TestSetstatePortConfigMergedNotEchoed(t *testing.T) {
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true}
	s := NewSession(testDesc(), "http://192.0.2.1:8080/inform", st, nil, time.Now())
	s.SetSnapshot(testSnapshot())
	body := `{"_type":"setstate","cfgversion":"v2","port_table":[{"port_idx":49,"name":"Uplink to agg","portconf_id":"abc"}],"port_overrides":[{"port_idx":1,"name":"x"}]}`
	s.Apply(time.Now(), []byte(body))
	m := payload(t, s)
	if m["cfgversion"] != "v2" {
		t.Errorf("cfgversion = %v", m["cfgversion"])
	}
	p49 := m["port_table"].([]any)[48].(map[string]any)
	if p49["name"] != "Uplink to agg" || p49["portconf_id"] != "abc" || p49["up"] != true || p49["rx_bytes"] != float64(111) {
		t.Errorf("pushed config not merged over live port 49: %v", p49)
	}
	if _, ok := m["port_overrides"]; !ok {
		t.Error("port_overrides must be echoed back")
	}
}

func TestMgmtCfgAdoptsOnlyFromDefaultKey(t *testing.T) {
	s := NewSession(testDesc(), "http://192.0.2.1:8080/inform", State{}, nil, time.Now())
	first := `{"_type":"setparam","mgmt_cfg":"cfgversion=a\nauthkey=0123456789abcdef0123456789abcdef\nuse_aes_gcm=true\n"}`
	s.Apply(time.Now(), []byte(first))
	if !s.Adopted() || !s.UseAESGCM() || s.AuthKey() != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("first mgmt_cfg not adopted: adopted=%v gcm=%v key=%s", s.Adopted(), s.UseAESGCM(), s.AuthKey())
	}
	replay := `{"_type":"setparam","mgmt_cfg":"cfgversion=b\nauthkey=00000000000000000000000000000000\n"}`
	s.Apply(time.Now(), []byte(replay))
	if s.AuthKey() != "0123456789abcdef0123456789abcdef" {
		t.Errorf("replayed mgmt_cfg clobbered the adopted key: %s", s.AuthKey())
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	store := &Store{Path: filepath.Join(t.TempDir(), "sub", "device.json")}
	s := NewSession(testDesc(), "http://192.0.2.1:8080/inform", State{}, store, time.Now())
	s.Apply(time.Now(), []byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=a\nauthkey=0123456789abcdef0123456789abcdef\nuse_aes_gcm=true\n"}`))
	s.Apply(time.Now(), []byte(`{"_type":"setstate","port_overrides":[{"port_idx":1}]}`))

	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Adopted || !st.UseAESGCM || st.Key != "0123456789abcdef0123456789abcdef" || st.CfgVersion != "a" || len(st.Provisioned["port_overrides"]) == 0 {
		t.Errorf("persisted state = %+v", st)
	}
	s2 := NewSession(testDesc(), "http://ignored:8080/inform", st, store, time.Now())
	if s2.InformURL() != "http://192.0.2.1:8080/inform" || !s2.Adopted() || !s2.UseAESGCM() {
		t.Errorf("resumed session = url %s adopted %v gcm %v", s2.InformURL(), s2.Adopted(), s2.UseAESGCM())
	}
	if _, err := (&Store{Path: filepath.Join(t.TempDir(), "missing.json")}).Load(); err != nil {
		t.Errorf("missing state file must load as fresh: %v", err)
	}
}

func TestSystemCfgStaysPendingUntilApplied(t *testing.T) {
	store := &Store{Path: filepath.Join(t.TempDir(), "device.json")}
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, CfgVersion: "old"}
	s := NewSession(testDesc(), "http://192.0.2.1:8080/inform", st, store, time.Now())
	push := `{"_type":"setparam","cfgversion":"new1","mgmt_cfg":"cfgversion=new1\n","system_cfg":"switch.port.2.status=disabled\n"}`
	effects := s.Apply(time.Now(), []byte(push))
	var sawCfg bool
	for _, e := range effects {
		if e.Kind == EffectSystemCfg && e.Text == "new1" {
			sawCfg = true
		}
	}
	if !sawCfg {
		t.Fatalf("no EffectSystemCfg in %+v", effects)
	}
	if m := payload(t, s); m["cfgversion"] != "old" {
		t.Errorf("cfgversion reported %v before apply, want old", m["cfgversion"])
	}
	ver, text, ok := s.Pending()
	if !ok || ver != "new1" || text != "switch.port.2.status=disabled\n" {
		t.Fatalf("pending = %q %q %v", ver, text, ok)
	}
	loaded, _ := store.Load()
	if loaded.PendingCfgVersion != "new1" {
		t.Errorf("pending push not persisted: %+v", loaded)
	}
	if err := s.MarkApplied("stale"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Pending(); !ok {
		t.Error("MarkApplied with a stale version must not clear the pending push")
	}
	if err := s.MarkApplied("new1"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Pending(); ok {
		t.Error("still pending after MarkApplied")
	}
	if m := payload(t, s); m["cfgversion"] != "new1" {
		t.Errorf("cfgversion reported %v after apply", m["cfgversion"])
	}
	loaded, _ = store.Load()
	if loaded.CfgVersion != "new1" || loaded.SystemCfg == "" || loaded.PendingSystemCfg != "" {
		t.Errorf("persisted after apply: %+v", loaded)
	}
}

// The Version column in the UI must say what is really running on the
// switch (EOS 4.26.14M, PVE 9.1.6), not the model profile's UniFi
// firmware. An operator's -version wins; an emulated upgrade holds until
// the switch itself is upgraded.
func TestFirmwareVersionIsTheSwitchsOwn(t *testing.T) {
	snap := testSnapshot()
	snap.System.Version = "4.26.14M"
	desc, err := DescriptorFor("UDC48X6", snap, Identity{MAC: snap.System.MAC})
	if err != nil {
		t.Fatal(err)
	}
	if desc.Version != "4.26.14M" {
		t.Errorf("reported version = %q, want the switch's own", desc.Version)
	}
	if d, _ := DescriptorFor("UDC48X6", snap, Identity{MAC: snap.System.MAC, Version: "9.9.9"}); d.Version != "9.9.9" {
		t.Errorf("-version = %q, want the operator's", d.Version)
	}
	if d, _ := DescriptorFor("UDC48X6", nil, Identity{MAC: snap.System.MAC}); d.Version == "" || d.Version == "4.26.14M" {
		t.Errorf("without a switch the model profile's version must be reported, got %q", d.Version)
	}

	// An emulated upgrade (the controller cannot flash this switch) is
	// reported until the switch's own version moves.
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, Firmware: "7.3.109.16640", FirmwareBase: "4.26.14M"}
	s := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	s.SetSnapshot(snap)
	if got := s.Version(); got != "7.3.109.16640" {
		t.Errorf("after an emulated upgrade: %q", got)
	}
	upgraded := testSnapshot()
	upgraded.System.Version = "4.27.0F"
	s.SetSnapshot(upgraded)
	if got := s.Version(); got != "4.27.0F" {
		t.Errorf("after a real upgrade the truth must win, got %q", got)
	}
	// And it stays won across a restart from the persisted state.
	if got := NewSession(desc, "http://192.0.2.1:8080/inform", State{Firmware: "7.3.109.16640", FirmwareBase: "4.26.14M"}, nil, time.Now()); got.Version() != "7.3.109.16640" {
		t.Errorf("a restart with the same switch version = %q", got.Version())
	}
	d2, _ := DescriptorFor("UDC48X6", upgraded, Identity{MAC: snap.System.MAC})
	if got := NewSession(d2, "http://192.0.2.1:8080/inform", State{Firmware: "7.3.109.16640", FirmwareBase: "4.26.14M"}, nil, time.Now()); got.Version() != "4.27.0F" {
		t.Errorf("a restart after a real upgrade = %q", got.Version())
	}
	// A pinned version follows neither.
	pinned, _ := DescriptorFor("UDC48X6", snap, Identity{MAC: snap.System.MAC, Version: "9.9.9"})
	p := NewSession(pinned, "http://192.0.2.1:8080/inform", State{}, nil, time.Now())
	p.PinVersion()
	p.SetSnapshot(upgraded)
	if got := p.Version(); got != "9.9.9" {
		t.Errorf("pinned version = %q", got)
	}
}
