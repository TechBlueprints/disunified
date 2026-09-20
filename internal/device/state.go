package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// State is everything the controller gave the device that must survive a
// restart. Losing Key after adoption means the controller no longer
// recognises the device's informs and it has to be forgotten and re-adopted.
type State struct {
	Key         string                     `json:"authkey"`
	CfgVersion  string                     `json:"cfgversion"`
	Adopted     bool                       `json:"adopted"`
	UseAESGCM   bool                       `json:"use_aes_gcm"`
	InformURL   string                     `json:"inform_url"`
	Provisioned map[string]json.RawMessage `json:"provisioned,omitempty"` // setstate tables, echoed back

	// SystemCfg is the last system_cfg fully applied to the switch, and
	// CfgVersion above is its version. A push that has not been applied yet
	// waits in Pending*; the device keeps reporting the old CfgVersion until
	// it is, so the controller keeps re-sending rather than believing a lie.
	SystemCfg         string `json:"system_cfg,omitempty"`
	PendingCfgVersion string `json:"pending_cfgversion,omitempty"`
	PendingSystemCfg  string `json:"pending_system_cfg,omitempty"`

	// Firmware is the version the controller last "upgraded" the device to.
	// The controller cannot manage firmware on a bridged switch, so an
	// upgrade is accepted, the reboot emulated, and this version reported
	// from then on (persisted, so a restart does not look like a downgrade).
	Firmware string `json:"firmware,omitempty"`
}

func (st State) clone() State {
	c := st
	if st.Provisioned != nil {
		c.Provisioned = make(map[string]json.RawMessage, len(st.Provisioned))
		for k, v := range st.Provisioned {
			c.Provisioned[k] = v
		}
	}
	return c
}

func (st State) equal(o State) bool {
	if st.Key != o.Key || st.CfgVersion != o.CfgVersion || st.Adopted != o.Adopted ||
		st.UseAESGCM != o.UseAESGCM || st.InformURL != o.InformURL || len(st.Provisioned) != len(o.Provisioned) ||
		st.SystemCfg != o.SystemCfg || st.PendingCfgVersion != o.PendingCfgVersion || st.PendingSystemCfg != o.PendingSystemCfg ||
		st.Firmware != o.Firmware {
		return false
	}
	for k, v := range st.Provisioned {
		if !bytes.Equal(v, o.Provisioned[k]) {
			return false
		}
	}
	return true
}

// Store persists State as JSON at Path, mode 0600 (it holds the auth key).
type Store struct {
	Path string
}

// Load reads the state file. A missing file is a fresh device, not an error.
func (s *Store) Load() (State, error) {
	var st State
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("state file %s: %w", s.Path, err)
	}
	return st, nil
}

// Save writes atomically (temp file + rename).
func (s *Store) Save(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}
