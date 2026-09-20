// Package switchmodel is the vendor-neutral picture of a switch that the
// UniFi presentation layer consumes. Every vendor collector (Arista EOS
// today, others later) produces a Snapshot; nothing UniFi-specific lives here
// and nothing vendor-specific leaks past it.
package switchmodel

import (
	"context"
	"time"
)

// Collector produces a fresh Snapshot of one switch on demand.
type Collector interface {
	Collect(ctx context.Context) (*Snapshot, error)
}

// Controller writes UniFi-originated settings to the switch. Implementations
// must be idempotent: compare each desired value with the switch's current
// state and only write the differences, so re-applying the same config is
// a no-op and a stale config never flaps a port.
type Controller interface {
	// ApplyPorts makes the listed ports match desired. It returns how many
	// ports it actually changed.
	ApplyPorts(ctx context.Context, desired []PortDesired) (changed int, err error)
}

// PortDesired is the UniFi-side intent for one port. Description "" means
// "no description". SpeedMbps 0 means auto-negotiate. VLANs: TaggedAll
// means every VLAN tagged (plus NativeVLAN untagged); otherwise exactly
// TaggedVLANs are tagged; NativeVLAN 0 with no tagged VLANs is invalid and
// is skipped.
type PortDesired struct {
	Index       int
	Enabled     bool
	Description string

	SpeedMbps int // 0 = auto

	VLANSet     bool // false = leave VLAN config alone
	NativeVLAN  int
	TaggedAll   bool
	TaggedVLANs []int

	FEC       *FEC              // nil = leave the switch's FEC config alone
	StormCtrl *StormControlSpec // nil = no storm control (remove any)
	BPDUGuard bool
	STPEdge   bool // "STP disabled" on the port in UniFi terms: edge port, no BPDUs

	LAG          int // >0: member of link aggregation group LAG (every lane of a cage joins)
	MirrorSource int // >0: this port is a mirror destination receiving a copy of port MirrorSource's traffic
}

// StormControlSpec is per-traffic-type suppression in percent of link
// rate; a nil pointer means that type is not limited.
type StormControlSpec struct {
	BroadcastPct      *float64
	MulticastPct      *float64
	UnknownUnicastPct *float64
}

// SwitchDesired is the UniFi-side intent for switch-wide settings.
type SwitchDesired struct {
	STPSet      bool
	STPEnabled  bool
	STPMode     string // "rstp" | "stp" | "mstp"
	STPPriority int

	// IGMPSnooping: VLAN ID -> enabled, only for VLANs to manage.
	IGMPSnooping map[int]bool

	NTPServers   []string // nil = leave alone; empty = remove managed servers
	SyslogHosts  []string // same
	ManageNTP    bool
	ManageSyslog bool

	DHCPSnooping *bool // nil = leave alone; else enable/disable (on every VLAN the switch has)

	// SNMPCommunity: v1/v2c read-only community to configure; "" with SNMPSet = remove the managed one.
	SNMPSet       bool
	SNMPCommunity string
}

// PortCycler bounces a port (shutdown, wait, no shutdown): the controller's
// "port-cycle" command.
type PortCycler interface {
	CyclePort(ctx context.Context, idx int) error
}

// Rebooter really restarts the switch: the controller's "reboot" command
// when the operator has opted in (-control-reboot).
type Rebooter interface {
	Reboot(ctx context.Context) error
}

// SSHKey is one public key the controller pushes (sshd.auth.key.N.*).
type SSHKey struct {
	Type    string // "ssh-rsa", "ssh-ed25519"
	Value   string // base64 blob
	Comment string
}

// SSHKeyInstaller puts the controller's SSH keys on the switch so the
// controller (and the site's configured keys) can reach it.
type SSHKeyInstaller interface {
	InstallSSHKeys(ctx context.Context, keys []SSHKey) (installed int, err error)
}

// SwitchController applies switch-wide settings.
type SwitchController interface {
	ApplySwitch(ctx context.Context, desired SwitchDesired) (changed int, err error)
}

// Controller implementations also need the site VLAN list to exist on the
// switch before ports can reference it.
type VLANController interface {
	EnsureVLANs(ctx context.Context, ids []int) (created int, err error)
}

// Snapshot is the state of one switch at one instant.
type Snapshot struct {
	TakenAt  time.Time
	System   System
	Ports    []Port     // sorted by Index, one entry per front-panel port
	MACTable []MACEntry // every learned address, including those on non-front-panel ports
	VLANs    []int      // VLAN IDs that exist on the switch, ascending

	// UplinkHint is the driver's own idea of the uplink port when LLDP is
	// silent (a virtual switch knows which physical NIC carries it); 0 = none.
	UplinkHint int
}

// UplinkPort returns the port carrying the LLDP neighbour that looks like
// the upstream switch (a bridge that is also a router wins, else the first
// bridge), or 0 if none.
func (s *Snapshot) UplinkPort() int {
	best := 0
	for _, p := range s.Ports {
		if p.Neighbor == nil || !p.Neighbor.IsBridge || !p.Up {
			continue
		}
		if p.Neighbor.IsRouter {
			return p.Index
		}
		if best == 0 {
			best = p.Index
		}
	}
	if best == 0 {
		best = s.UplinkHint
	}
	return best
}

// System is the switch's identity and health.
type System struct {
	Vendor  string
	Model   string
	Serial  string
	MAC     string // primary/system MAC, lower-case colon form
	MgmtMAC string // the out-of-band management interface's own MAC, "" if none/unknown (UniFi's "service" interface)

	// OOBInterfaces lists dedicated out-of-band management interfaces. A
	// UniFi controller expects a switch's management address in-band, behind
	// its uplink; an address on an OOB port leaves the switch without a place
	// in the topology (see docs/adding-a-switch.md). Reported so the loop can
	// warn loudly.
	OOBInterfaces []OOBInterface
	Version       string // vendor's own version string, e.g. "4.26.14M"
	Hostname      string
	Uptime        time.Duration

	CPUPercent  float64 // 0-100, whole system
	MemTotalKB  uint64
	MemUsedKB   uint64
	MemBufferKB uint64

	TemperatureC   float64
	HasTemperature bool
	Overheating    bool

	Fans []Fan
	PSUs []PSU

	STPMode     string // "rstp", "mstp", "stp", "none", ""
	STPPriority int    // bridge priority, 0 if unknown
	STPRoot     string // MAC of the root bridge (this switch's own when it is the root), "" if unknown

	IGMPSnooping    map[int]bool // VLAN ID -> snooping enabled
	NTPServers      []string
	SyslogHosts     []string
	DHCPSnooping    bool
	SNMPCommunities []string // configured v1/v2c communities
}

// OOBInterface is one out-of-band management interface.
type OOBInterface struct {
	Name string
	Up   bool   // link up (cabled and active)
	IP   string // configured IPv4 address, "" if none
}

// PortVLAN is a port's configured 802.1Q state.
type PortVLAN struct {
	Mode       string // "trunk" | "access" | ""
	NativeVLAN int    // trunk native / access VLAN
	AllowAll   bool   // trunk carries every VLAN
	Allowed    []int  // trunk allowed VLANs when !AllowAll (sorted, includes native if allowed)
}

// Fan is one fan's state.
type Fan struct {
	Label    string
	SpeedPct int
	OK       bool
}

// PSU is one power supply's state.
type PSU struct {
	Slot      string
	Model     string
	Present   bool // a supply is fitted in the slot
	OK        bool // present and delivering power
	OutputW   float64
	CapacityW float64
	TempsC    []float64 // the supply's own temperature sensors, if any
}

// MACEntry is one learned MAC address.
type MACEntry struct {
	MAC       string
	VLAN      int
	PortIndex int       // front-panel port it was learned on; 0 = CPU/other
	LastMove  time.Time // when it was learned or last moved; zero if unknown
}

// Optic describes an installed transceiver and its DOM readings.
type Optic struct {
	Vendor     string
	Part       string
	Serial     string
	MediaType  string  // vendor string, e.g. "100GBASE-CWDM4"
	TempC      float64 // 0 when the module has no DOM
	VoltageV   float64
	TxBiasMA   float64
	TxPowerDBm float64
	RxPowerDBm float64
	HasDOM     bool
}

// FEC is the forward-error-correction state of a port.
type FEC string

const (
	FECUnknown  FEC = ""
	FECDisabled FEC = "disabled"
	FECRS       FEC = "rs-fec" // Reed-Solomon (CL91/CL108)
	FECFC       FEC = "fc-fec" // fire-code / BASE-R (CL74)
)

// Media is the physical port type, in a vendor-neutral vocabulary that maps
// one-to-one onto what the UniFi controller can render.
type Media string

const (
	MediaUnknown   Media = ""
	MediaCopper1G  Media = "1G-T"
	MediaCopper2G5 Media = "2.5G-T"
	MediaCopper10G Media = "10G-T"
	MediaSFP       Media = "SFP"
	MediaSFPPlus   Media = "SFP+"
	MediaSFP28     Media = "SFP28"
	MediaQSFPPlus  Media = "QSFP+"
	MediaQSFP28    Media = "QSFP28"
)

// Port is one front-panel port. A breakout slot (one physical cage split
// into several lanes) is folded into a single Port with Lanes > 1.
type Port struct {
	Index       int      // 1-based front-panel number
	IfName      string   // vendor interface name, e.g. "Ethernet49/1"
	Interfaces  []string // every vendor interface folded into this port (lanes), for config writes
	Name        string   // operator-facing name: the description if set, else IfName
	Description string
	Media       Media
	Lanes       int // 1, or the number of lanes a breakout slot was folded from

	Present    bool   // an optic/cable is present (always true for copper)
	Enabled    bool   // administratively up
	Fault      string // vendor fault state keeping the port down, e.g. "errdisabled: speed-misconfigured"; "" = none
	Up         bool   // link up
	SpeedMbps  int
	FullDuplex bool
	MTU        int

	Counters Counters
	STPState string // "forwarding", "blocking", "learning", "listening", "disabled", ""

	Neighbor *Neighbor // LLDP neighbour, nil if none

	AutoNeg      bool
	AdminSpeed   int   // configured forced speed in Mbps, 0 = auto
	LanesDiverge bool  // a breakout's lanes carry different config (e.g. a fresh split); forces a rewrite
	SpeedCaps    []int // speeds (Mbps) the port/optic can run, ascending; nil = unknown
	FECCapable   bool  // the port/optic supports forward error correction
	VLAN         PortVLAN
	FlowCtrlRx   bool
	FlowCtrlTx   bool
	STPPathCost  int
	LAG          string // aggregate the port belongs to ("Port-Channel1"), "" if none
	LAGID        int    // numeric id of LAG, 0 if none
	MirrorFrom   int    // >0: configured as a mirror destination for that source port
	FEC          FEC    // operating
	FECConfig    FEC    // configured (from running-config); "" = not configured
	BPDUGuard    bool
	STPEdge      bool              // portfast + bpdufilter configured
	StormCtrl    *StormControlSpec // nil = none configured
	Optic        *Optic            // nil for copper or an empty cage
	MACs         []MACEntry        // addresses learned on this port
	Health       PortHealth        // fault and change signals (anomaly reporting)
}

// PortHealth carries the vendor-neutral signals the presentation layer turns
// into per-port anomaly flags. Counters are lifetime values; the consumer
// keeps history and looks at growth.
type PortHealth struct {
	LinkChanges     uint64 // link state transitions
	STPChanges      int    // spanning-tree topology changes seen on this port
	STPInconsistent bool   // an STP guard (loop/root/bridge assurance) holds the port inconsistent
	OpticRxAlarm    bool   // receive power outside the optic's alarm thresholds
	OpticTxAlarm    bool   // transmit power or laser bias outside the alarm thresholds
	HasFECCounters  bool   // FECCorrected/FECUncorrected are populated
	FECCorrected    uint64 // FEC corrected codewords
	FECUncorrected  uint64 // FEC uncorrected codewords (each one is a lost frame)
	HasPCSCounters  bool   // PCSErrBlocks/PCSHighBER are populated (PHY layer; copper and optical alike)
	PCSErrBlocks    uint64 // PCS errored blocks, summed over lanes
	PCSHighBER      bool   // the PHY currently reports a high bit-error rate
}

// Counters are lifetime interface counters.
type Counters struct {
	RxBytes   uint64
	TxBytes   uint64
	RxPackets uint64
	TxPackets uint64
	RxErrors  uint64
	TxErrors  uint64
	RxDropped uint64
	TxDropped uint64

	RxMulticast uint64
	RxBroadcast uint64
	TxMulticast uint64
	TxBroadcast uint64
}

// Neighbor is what LLDP reports on the far end of a port.
type Neighbor struct {
	SystemName      string
	SystemDesc      string
	ChassisID       string
	PortID          string
	PortDescription string
	ManagementIP    string
	IsBridge        bool
	IsRouter        bool
}

// Add sums c2 into c (used when folding breakout lanes).
func (c *Counters) Add(c2 Counters) {
	c.RxBytes += c2.RxBytes
	c.TxBytes += c2.TxBytes
	c.RxPackets += c2.RxPackets
	c.TxPackets += c2.TxPackets
	c.RxErrors += c2.RxErrors
	c.TxErrors += c2.TxErrors
	c.RxDropped += c2.RxDropped
	c.TxDropped += c2.TxDropped
	c.RxMulticast += c2.RxMulticast
	c.RxBroadcast += c2.RxBroadcast
	c.TxMulticast += c2.TxMulticast
	c.TxBroadcast += c2.TxBroadcast
}

// Capabilities is what a driver can honour on its switch. The presentation
// layer turns it into the controller's capability claims, which gate the
// controls the UI offers, so a driver claims only what it implements: a
// claimed feature the driver ignores is a silent lie in the UI.
type Capabilities struct {
	STP           bool // switch-wide STP mode/priority
	BPDUGuard     bool // per-port BPDU guard
	STPPortCost   bool // per-port STP path cost is reported
	Jumbo         bool // jumbo frames (reported and/or switchable)
	FEC           bool // per-port forward error correction
	LACP          bool // link aggregation (Controller honours PortDesired.LAG)
	StormControl  bool // per-port storm control in percent
	IGMPSnooping  bool // IGMP snooping (SwitchController honours IGMPSnooping)
	LLDPMED       bool
	DHCPSnooping  bool
	PortIsolation bool
	SNMP          bool // SwitchController honours SNMPCommunity
	// MirrorSessions / AggregateSessions are the counts the UI is told; 0
	// hides the feature.
	MirrorSessions    int
	AggregateSessions int
}

// Capable is implemented by a Switch that wants to declare its own
// Capabilities; a Switch without it gets the presentation layer's default
// (the Arista EOS set).
type Capable interface {
	Capabilities() Capabilities
}
