package informloop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// LayoutFile is the name of the port-layout file in a device's state
// directory (Config.StateDir), next to device.json and never inside it.
const LayoutFile = "layout.json"

// layoutRecord is what the loop last compared the controller's config
// against: the layout signature, each port's identity (portIdentities) and
// the ports still fresh (Loop.freshPorts). It is the baseline of the next
// run: a port whose identity differs from it at that run's first collect is
// fresh, whether it changed between the start and the first collect or
// while the bridge was stopped, and a port withheld when the bridge stopped
// is withheld when it starts again.
type layoutRecord struct {
	Signature string         `json:"signature"`
	Ports     map[int]string `json:"ports"`
	Fresh     map[int]string `json:"fresh,omitempty"`
}

// portIdentities is each port's identity for freshness: its interface name
// plus the interfaces behind it. A guest that migrates onto this node keeps
// its port number and name but gains its tap here, and must be treated as
// fresh: this device's controller config for the port may differ from the
// guest's own. A port without an interface name has no occupant.
func portIdentities(snap *devicemodel.Snapshot) map[int]string {
	byPort := map[int]string{}
	for _, p := range snap.Ports {
		if p.IfName == "" {
			continue
		}
		byPort[p.Index] = p.IfName + "/" + strings.Join(p.Interfaces, ",")
	}
	return byPort
}

// seedLayout sets the baseline the first collect of the run is compared
// with. Without one the first collect was never a change, so a guest that
// arrived between the startup snapshot (the one the controller was
// provisioned from) and the first collect got the stored push for its port.
// The baseline is the previous run's record when the state directory holds
// one, the startup snapshot otherwise (a first run, or no state directory);
// start is nil for a loop without a device. Nothing is logged: a run whose
// first collect equals the baseline behaves as it did before.
func (l *Loop) seedLayout(start *devicemodel.Snapshot) {
	if l.cfg.StateDir != "" {
		path := filepath.Join(l.cfg.StateDir, LayoutFile)
		b, err := os.ReadFile(path)
		var rec layoutRecord
		if err == nil {
			err = json.Unmarshal(b, &rec)
		}
		switch {
		case err == nil && rec.Signature != "":
			l.layoutSig, l.layoutByPort = rec.Signature, rec.Ports
			if len(rec.Fresh) > 0 {
				l.freshPorts = rec.Fresh
			}
			return
		case err != nil && !errors.Is(err, os.ErrNotExist):
			l.cfg.Logger.Printf("[%s] port layout %s not loaded, starting from the startup snapshot: %v", l.desc.MAC, path, err)
		}
	}
	if start == nil {
		return
	}
	l.layoutSig, l.layoutByPort = layoutSignature(start), portIdentities(start)
	l.saveLayout()
}

// saveLayout writes the layout record (no state directory: nothing). A
// failure is logged once per distinct error; the record stays in memory,
// and the next run starts from its own startup snapshot.
func (l *Loop) saveLayout() {
	if l.cfg.StateDir == "" {
		return
	}
	err := l.writeLayout()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != l.layoutSaveErr {
		l.layoutSaveErr = msg
		if err != nil {
			l.cfg.Logger.Printf("[%s] port layout not saved (held in memory; ports withheld now are not withheld after a restart): %v", l.desc.MAC, err)
		}
	}
}

func (l *Loop) writeLayout() error {
	b, err := json.MarshalIndent(layoutRecord{Signature: l.layoutSig, Ports: l.layoutByPort, Fresh: l.freshPorts}, "", " ")
	if err != nil {
		return err
	}
	path := filepath.Join(l.cfg.StateDir, LayoutFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}
