package devicemodel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Lane switches (docs/drivers/breakout.md): a device whose cage is broken
// out into lanes can present those lanes as a small switch of their own --
// a second device, in the same process, that reads the lanes from this
// device's last snapshot and writes them through this device's own plan.
// Nothing here is UniFi- or vendor-specific: lanes are Ports and their
// intent is PortDesired, indexed 1..N in the device's lane order.

// LaneProvider is a device whose split cages can be presented as lane
// switches.
type LaneProvider interface {
	// Lanes is the cage as of the device's last snapshot: the cage as it
	// folds it, each lane as a Port of its own (Index = lane number 1..N,
	// Interfaces = its one interface), and the device's identity. An error
	// when there is no snapshot yet, no such cage, or the cage is not split
	// (one lane) -- the lane switch then does not answer.
	Lanes(cage int) (LaneView, error)
	// ApplyLanes makes the cage's lanes match desired (Index = lane number):
	// a diff against the device's state, idempotent, through the device's
	// own write path. PlanLanes reports which lanes ApplyLanes would change
	// for the controller's config, writing nothing (the first-push hold).
	ApplyLanes(ctx context.Context, cage int, desired []PortDesired) (changed int, err error)
	PlanLanes(cage int, desired []PortDesired) (changed []int)
	// AttachLaneSwitch declares a lane switch on the cage. From then on,
	// while the cage is split, the device reports the child's MAC in the
	// cage's MAC table and the child as the cage's LLDP neighbour (Child
	// set), and its own plan for the cage keeps only the breakout (the
	// cage's speed) and the cage's admin state: the lanes' other settings
	// are the child's (ApplyLanes). A whole cage is the device's as before.
	AttachLaneSwitch(cage int, child Attachment) error
	// DetachLaneSwitch is the exact inverse: the cage is the device's again,
	// reported and planned as the fold it was before the attach (from the
	// next collect and the next plan). What the lane switch left on the
	// lanes falls under the device's rules for a cage with no lane switch.
	// Detaching a cage with none attached does nothing.
	DetachLaneSwitch(cage int)
	// SplitCages lists the port indexes of the cages the last snapshot saw
	// split, ascending; nil before the first snapshot. Each can be presented
	// as a lane switch (Lanes answers for it).
	SplitCages() []int
}

// LaneVLANController is an optional LaneProvider capability: the VLANs a
// lane switch's own push names, kept apart from the device's. A
// controller's push names only the VLANs that device's ports carry, so a
// lane switch's list can differ from its parent's, and a parent that
// deletes the VLANs its push no longer names (sonic, arista-eos) must keep the ones an
// attached lane switch still carries. Same contract as
// VLANController.EnsureVLANs otherwise.
type LaneVLANController interface {
	EnsureLaneVLANs(ctx context.Context, cage int, ids []int) (created int, err error)
}

// Attachment is a declared lane switch.
type Attachment struct {
	MAC  string // lower-case colon form
	Name string // the child's name, reported as the LLDP system name
}

// LaneView is one split cage as a lane switch sees it.
type LaneView struct {
	TakenAt   time.Time // the parent snapshot's
	System    System    // the parent's identity and reachability
	Cage      Port      // the cage as the parent reports it
	CageAlias string    // the parent's faceplate label for the cage ("Eth2"); "" = none
	Lanes     []Port    // Index 1..N
	// LaneAliases are the faceplate labels of the lanes ("Eth2/1"), one per
	// lane; "" where the device has none.
	LaneAliases []string
	// ChassisID is what the parent reports as its own LLDP chassis id (what
	// a neighbour of it sees); "" = unknown (the system MAC stands in).
	ChassisID string
	// Mode names the cage's lane layout ("4x25G[10G]"), "" = unknown.
	Mode string
	// CageDisabled: the parent's own config has the cage administratively
	// down, which takes every lane down whatever the lane switch asks.
	CageDisabled bool
	// VLANs are the VLAN ids the parent has (its Snapshot.VLANs).
	VLANs []int
	// Child is the lane switch attached to the cage (AttachLaneSwitch);
	// zero when none is.
	Child Attachment
}

// LaneSwitchMAC is the MAC a lane switch reports when none is configured:
// the parent's MAC with the locally administered bit set (and the multicast
// bit clear) in the first octet and the cage number in the last. Stable
// across restarts and distinct per cage of one parent; never a vendor's
// address, since a locally administered MAC has no OUI.
func LaneSwitchMAC(parentMAC string, cage int) (string, error) {
	hw, err := net.ParseMAC(parentMAC)
	if err != nil || len(hw) != 6 {
		return "", fmt.Errorf("parent MAC %q is not a 6-byte MAC", parentMAC)
	}
	if cage < 1 || cage > 255 {
		return "", fmt.Errorf("cage %d: must be 1..255", cage)
	}
	m := append(net.HardwareAddr(nil), hw...)
	m[0] = (m[0] | 0x02) &^ 0x01
	m[5] = byte(cage)
	return m.String(), nil
}

// The opened devices of this process, by config name: how a lane switch
// finds its parent. Every device runs in one process (cmd/disunified
// runConfig), so a registry by name is all the parent/child link needs.
var (
	openedMu sync.Mutex
	opened   = map[string]Device{}
)

// RegisterOpened records an opened device under its config name (main,
// once Start has succeeded); UnregisterOpened removes it again on exit.
func RegisterOpened(name string, d Device) {
	openedMu.Lock()
	defer openedMu.Unlock()
	opened[name] = d
}

// UnregisterOpened removes name, if it still maps to d.
func UnregisterOpened(name string, d Device) {
	openedMu.Lock()
	defer openedMu.Unlock()
	if opened[name] == d {
		delete(opened, name)
	}
}

// OpenedDevice is the device opened under name, if any.
func OpenedDevice(name string) (Device, bool) {
	openedMu.Lock()
	defer openedMu.Unlock()
	d, ok := opened[name]
	return d, ok
}
