// Package devicemodel is the vendor-neutral picture of a device that the
// UniFi presentation layer consumes. Every vendor collector (Arista EOS
// today, others later) produces a Snapshot; nothing UniFi-specific lives here
// and nothing vendor-specific leaks past it.
package devicemodel

import (
	"context"
	"fmt"
	"strings"
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
	// STPEdgeMode is the port's Edge Port setting: "auto" (edge once no BPDU
	// arrives), "enabled" (edge from link up), "disabled" (never edge); ""
	// when the controller did not push it. Only a driver claiming
	// Capabilities.STPAutoEdge reads it.
	STPEdgeMode string
	// LoopProtect is UniFi's Loop Protection on the port; only a driver
	// claiming Capabilities.LoopProtection reads it.
	LoopProtect bool

	LAG int // >0: member of link aggregation group LAG (every lane of a cage joins)
	// Routed: the port -- or, with LAG, its aggregate -- is a routed
	// interface (UniFi's Routing and Routing + Aggregating operations):
	// it carries no VLAN, and its address is an L3Interface's
	// (DeviceDesired.L3Interfaces). The VLAN fields are ignored.
	Routed       bool
	MirrorSource int // >0: this port is a mirror destination receiving a copy of port MirrorSource's traffic

	// MTU is the controller's switch-wide jumbo-frames setting as a port
	// MTU: switch.mtu (9216 when absent) with switch.jumboframes enabled,
	// 1500 with it disabled; 0 = not in the push, leave the MTU alone. Only
	// a JumboSwitcher reads it.
	MTU int

	// Isolation: UniFi's port isolation -- the port reaches the switch's
	// other ports but no other isolated one. Read by a driver claiming
	// Capabilities.PortIsolation.
	Isolation bool
	// PortSecurity: only the source MACs in AllowedMACs (lower-case,
	// colon form) may send through the port; with none listed, none. Read
	// by a driver claiming Capabilities.PortSecurity.
	PortSecurity bool
	AllowedMACs  []string
	// EgressKbps limits what the port sends to that many kbit/s; 0 = no
	// limit. Read by a driver claiming Capabilities.EgressRateLimit.
	EgressKbps int
	// LLDPMED is the port's LLDP-MED setting; nil = not in the push. Read
	// by a driver claiming Capabilities.LLDPMED.
	LLDPMED *bool
	// VoiceVLAN is the VLAN LLDP-MED's network policy advertises for voice
	// (UniFi's Voice VLAN); 0 = none. Read with LLDPMED.
	VoiceVLAN int
	// Dot1X is the port's 802.1X control mode while the device's 802.1X is
	// on (DeviceDesired.Dot1X); "" = no port control (802.1X off, or not
	// in the push). Read by a driver claiming Capabilities.Dot1X.
	Dot1X Dot1XMode
	// Dot1XIdleTimeout is how long, in seconds, a host admitted by its MAC
	// address (Dot1XMACBased) may stay idle before it must authenticate
	// again; 0 = not in the push.
	Dot1XIdleTimeout int
}

// Dot1XMode is a port's 802.1X control (IEEE 802.1X port access entity).
type Dot1XMode string

const (
	// Dot1XForceAuthorized: the port forwards, nothing authenticates.
	Dot1XForceAuthorized Dot1XMode = "force_authorized"
	// Dot1XForceUnauthorized: the port forwards nothing from any host.
	Dot1XForceUnauthorized Dot1XMode = "force_unauthorized"
	// Dot1XAuto (UniFi's Credentials): port-based -- once one host has
	// authenticated (EAP through the RADIUS servers), the port is open to
	// every host behind it, until the last authenticated one is gone (a
	// UniFi switch runs it as multi-host, docs/drivers/sonic.md §11.26.9).
	Dot1XAuto Dot1XMode = "auto"
	// Dot1XMultiHost (UniFi's Multiple Credentials): each host is admitted
	// once it authenticates; the others are not (multi-sessions on a UniFi
	// switch).
	Dot1XMultiHost Dot1XMode = "multi_host"
	// Dot1XMACBased: each host is admitted by its MAC address, which the
	// RADIUS servers accept or refuse.
	Dot1XMACBased Dot1XMode = "mac_based"
)

// Closed: the port drops a host's frames until it is admitted.
func (m Dot1XMode) Closed() bool {
	return m == Dot1XForceUnauthorized || m == Dot1XAuto || m == Dot1XMultiHost || m == Dot1XMACBased
}

// Dot1X is the device's 802.1X port authentication: the RADIUS servers its
// controlled ports authenticate against.
type Dot1X struct {
	Enabled bool
	Servers []RADIUSServer // in the order to try them
	// FallbackVLAN is where a port's hosts go when none authenticates; 0 =
	// none (they stay out).
	FallbackVLAN int
}

// RADIUSServer is one authentication server. Secret is a credential: a
// driver writes it where only the authenticator reads it and logs it
// nowhere.
type RADIUSServer struct {
	Addr   string
	Port   int
	Secret string
}

// LLDPMEDSwitcher is an optional Controller capability: the driver turns
// LLDP-MED on and off per port (PortDesired.LLDPMED). Without it the loop
// notes that a port's LLDP-MED off is left as the switch has it.
type LLDPMEDSwitcher interface {
	SwitchesLLDPMED() bool
}

// JumboSwitcher is an optional Controller capability: the driver honours
// PortDesired.MTU (it can turn jumbo frames off). Without it the loop
// notes that the switch forwards jumbo frames unconditionally.
type JumboSwitcher interface {
	SwitchesJumbo() bool
}

// StormControlSpec is per-traffic-type suppression in percent of link
// rate; a nil pointer means that type is not limited.
type StormControlSpec struct {
	BroadcastPct      *float64
	MulticastPct      *float64
	UnknownUnicastPct *float64
}

// DeviceDesired is the UniFi-side intent for switch-wide settings.
type DeviceDesired struct {
	STPSet      bool
	STPEnabled  bool
	STPMode     string // "rstp" | "stp" | "mstp"
	STPPriority int

	// IGMPSnooping: VLAN ID -> enabled, only for VLANs to manage.
	IGMPSnooping map[int]bool
	// IGMP carries, for the VLANs IGMPSnooping turns on, the rest of each
	// one's snooping settings where the controller sent them (nil = none
	// sent; a driver that honours only on/off ignores it). Read by a driver
	// claiming Capabilities.IGMPSnoopingAdvanced.
	IGMP map[int]IGMPVLAN
	// IGMPFloodReserved: the well-known multicast groups (SSDP, NTP, PTP,
	// H.323, SLP) reach every port of a snooping VLAN whatever its joins;
	// IGMPUnknownToRouters: unregistered multicast reaches the
	// multicast-router ports. nil = not in the push.
	IGMPFloodReserved    *bool
	IGMPUnknownToRouters *bool

	NTPServers   []string // nil = leave alone; empty = remove managed servers
	SyslogHosts  []string // same
	ManageNTP    bool
	ManageSyslog bool

	// Dot1X is the device's 802.1X port authentication; nil = not in the
	// push. Read by a driver claiming Capabilities.Dot1X, with each
	// PortDesired.Dot1X.
	Dot1X *Dot1X

	DHCPSnooping *bool // nil = leave alone; else enable/disable (on every VLAN the switch has)
	// DHCPGuards are, while DHCPSnooping is on, the DHCP servers trusted on
	// each VLAN: on a VLAN that has one, only those servers may answer. A
	// VLAN without one has no server list and is not guarded.
	DHCPGuards []DHCPGuard

	// IPACLs and MACACLs are the device's switch ACL rules, each kind in
	// the controller's order (the first rule that matches decides; no rule
	// = allowed). IPACLSet / MACACLSet false leaves the device's rules of
	// that kind as they are: the push held a rule that could not be
	// translated. Set with an empty list = no rules.
	IPACLSet  bool
	IPACLs    []IPACL
	MACACLSet bool
	MACACLs   []MACACL

	// BlockedClients are the MACs (lowercase, colon-separated) of the
	// clients the controller blocks (UniFi's client Block): every frame
	// from one is dropped, in every VLAN, the management VLAN and the
	// device's own addresses included, as UniFi's own SONiC switch does it
	// (docs/drivers/sonic.md §11.33). Kept apart from MACACLs: not an
	// operator's rule and not counted against Capabilities.MACACLs.
	// BlockedSet false = the controller has not said yet (no setparam since
	// adoption): the device keeps the blocks it has.
	BlockedSet     bool
	BlockedClients []string

	// SNMPCommunity: v1/v2c read-only community to configure; "" with SNMPSet = remove the managed one.
	SNMPSet       bool
	SNMPCommunity string

	// Timezone is the controller's system.timezone (locale.timezone when
	// that is absent): on Network 10.6.106 a POSIX TZ string such as
	// "MST7MDT,M3.2.0,M11.1.0", not a zone name. "" = not in the push, leave
	// the device alone. A driver maps it onto what its device takes.
	Timezone string

	// Hostname is the controller's resolv.host.1.name (the device's UniFi
	// name without spaces), set only when the operator opted in
	// (control.hostname); "" = leave the device's hostname alone.
	Hostname string

	// L3Set: the push said whether the switch routes (UniFi's Layer 3
	// switching, switch.routing_enabled); false leaves the device's routed
	// interfaces and routes alone. Set with empty lists = route nothing.
	// Read by a driver claiming Capabilities.L3Routing.
	L3Set        bool
	L3Interfaces []L3Interface
	StaticRoutes []StaticRoute

	// BGPSet: the push said whether the switch runs a BGP router; false
	// leaves the device's BGP alone. BGPConfig is the operator's FRR bgpd
	// configuration file, applied whole ("" with BGPSet = no BGP: the router
	// is off or gone). Read by a driver claiming Capabilities.BGP.
	BGPSet    bool
	BGPConfig string

	// OSPFSet: the push said whether the switch runs an OSPF router; false
	// leaves the device's OSPF alone. OSPF nil with OSPFSet = no OSPF. Read
	// by a driver claiming Capabilities.OSPF.
	OSPFSet bool
	OSPF    *OSPFRouter
}

// OSPFRouter is the switch's OSPF router: its id, whether it announces its
// default route, which routes it redistributes ("" = none, "e1"/"e2" = as
// external routes of that metric type), the areas and the routed networks
// in them.
type OSPFRouter struct {
	RouterID        string
	AnnounceDefault bool
	RedistStatic    string
	RedistConnected string
	Areas           []OSPFArea
	Interfaces      []OSPFInterface
}

// OSPFArea is an area id (dotted) and its type: "normal", "stub", "nssa".
type OSPFArea struct {
	ID   string
	Type string
}

// OSPFInterface is one routed network in an area, by its VLAN (UniFi's id).
// Auth is "" (none), "plaintext" (Password) or "md5" (MD5Keys).
type OSPFInterface struct {
	Name           string // the network's name, for logs
	VLAN           int
	Area           string
	Passive        bool
	Cost, Priority int
	Hello, Dead    int // seconds
	Auth           string
	Password       string
	MD5Keys        []OSPFKey
}

// OSPFKey is one MD5 key: its id and the key.
type OSPFKey struct {
	ID  int
	Key string
}

// L3Interface is one network the switch routes: its own address on the
// VLAN -- or on a routed port or aggregate --, the DHCP service it runs
// there and whether the network is isolated (UniFi's Isolate Network: no
// traffic to or from the switch's other networks). Exactly one of VLAN,
// Port and LAG is set.
type L3Interface struct {
	Name string // the network's name, for logs
	VLAN int    // UniFi's id (1 is the untagged Default network)
	// Port is a routed port (UniFi's port index): the port carries no
	// VLAN and the address is its own (PortDesired.Routed).
	Port int
	// LAG is a routed aggregate (PortDesired.LAG of its members, which
	// are Routed): the address is the aggregate's own.
	LAG      int
	Address  string // the switch's address with the prefix length, a.b.c.d/n
	Isolated bool
	DHCP     L3DHCP
}

// Where names the interface for logs: "VLAN 10", "port 31", "aggregate 1".
func (in L3Interface) Where() string {
	switch {
	case in.Port > 0:
		return fmt.Sprintf("port %d", in.Port)
	case in.LAG > 0:
		return fmt.Sprintf("aggregate %d", in.LAG)
	}
	return fmt.Sprintf("VLAN %d", in.VLAN)
}

// L3DHCP is a routed network's DHCP service: Mode "" = none, "server" =
// the switch leases Start..End itself, "relay" = it forwards to Relays.
type L3DHCP struct {
	Mode      string
	Start     string
	End       string
	Router    string   // the default gateway handed out
	DNS       []string // the name servers handed out
	LeaseSecs int
	Relays    []string
	// Static are the clients with a Fixed IP on the network: each MAC
	// always gets its IP from the switch's DHCP server.
	Static []DHCPStatic
}

// DHCPStatic is one fixed address: MAC (aa:bb:cc:dd:ee:ff) gets IP.
type DHCPStatic struct {
	MAC string
	IP  string
}

// StaticRoute is one route the switch installs: Prefix via NextHop, out
// of a routed interface (an interface route, no next hop: the network on
// VLAN, the routed Port or the routed aggregate LAG), or into nothing
// (Blackhole). Exactly one is set.
type StaticRoute struct {
	Name      string // for logs
	Prefix    string // a.b.c.d/n, network address
	NextHop   string
	VLAN      int
	Port      int
	LAG       int
	Blackhole bool
}

// IGMPVLAN is one snooping VLAN's settings beside on/off. Times are in
// seconds; 0 = not given.
type IGMPVLAN struct {
	Querier        bool   // the switch sends general queries on the VLAN
	QuerierAddress string // the source address for them; "" = none named
	FastLeave      bool   // a leave removes the port at once
	MaxResponse    int    // the max response time queries carry
	Membership     int    // the group membership interval
	RouterExpiry   int    // how long a learned multicast-router port lasts
	Suppression    bool   // report suppression
	// FloodUnknown: unregistered multicast floods the VLAN (true) or goes to
	// the multicast-router ports only (false); nil = not given.
	FloodUnknown *bool
	// RouterPorts are the ports the operator made static multicast-router
	// ports in this VLAN (the UI's Multicast Router Port), by index.
	RouterPorts []int
}

// DHCPGuard trusts one DHCP server on one VLAN (UniFi's id: 1 is the
// untagged Default network).
type DHCPGuard struct {
	VLAN   int
	Server string // IPv4 address
}

// IPACL is one ACL rule on IPv4 traffic entering the switch. Protocol ""
// matches every IPv4 packet; otherwise "tcp" or "udp".
type IPACL struct {
	Name     string // the operator's name for the rule, for logs only
	Permit   bool
	Protocol string
	Src, Dst IPACLEnd
}

// IPACLEnd is one side of an IP rule. Every field empty or zero matches
// anything.
type IPACLEnd struct {
	CIDR string // IPv4 prefix ("192.0.2.0/24"), "" = any address
	Port int    // one L4 port, 0 = any
	// PortMin..PortMax is an L4 port range (both 0 = none).
	PortMin, PortMax int
}

// MACACL is one ACL rule on traffic within one VLAN, by MAC address.
type MACACL struct {
	Name     string // the operator's name for the rule, for logs only
	Permit   bool
	VLAN     int // UniFi's id
	Src, Dst MACACLEnd
}

// MACACLEnd is one side of a MAC rule: MAC under Mask (both "" = any).
type MACACLEnd struct {
	MAC, Mask string
}

// Locator is an optional capability: the device can show where it is (the
// controller's set-locate / unset-locate, "Locate" in the UI) by its own
// LEDs. Without it the session only reports `locating`. SetLocate must
// return promptly; a blink that needs time runs in the background until
// SetLocate(ctx, false) or the device is closed.
type Locator interface {
	SetLocate(ctx context.Context, on bool) error
}

// PortCycler bounces a port (shutdown, wait, no shutdown): the controller's
// "port-cycle" command.
type PortCycler interface {
	CyclePort(ctx context.Context, idx int) error
}

// PortRecoverer re-enables a port the device's own protection holds down
// (BPDU guard, loop protection): the controller's "port-recovery" command,
// which the UI's Re-enable sends for such a port. A port nothing holds is
// left as it is.
type PortRecoverer interface {
	RecoverPort(ctx context.Context, idx int) error
}

// OutletCycler power-cycles one outlet on a power device: the controller's
// "relayctl" command, which the UI's per-outlet Power Cycle sends. Optional,
// discovered with a type assertion like PortCycler.
type OutletCycler interface {
	CycleOutlet(ctx context.Context, idx int) error
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

// DeviceController applies switch-wide settings.
type DeviceController interface {
	ApplyDevice(ctx context.Context, desired DeviceDesired) (changed int, err error)
}

// OutletDesired is the UniFi-side intent for one outlet on a power device.
type OutletDesired struct {
	Index int
	On    bool

	// Name is the operator's name for the outlet, which the controller owns:
	// a power device must not report outlet names back on its inform (the
	// controller merges its own onto the row and drops the whole table when
	// that merge changes nothing), so the name only ever travels
	// controller -> bridge -> device. "" means leave the device's name alone.
	Name string
}

// AddressDesired is the controller's intent for the device's own management
// address: the IP Settings the operator chooses for an adopted device. DHCP
// true means the device should take a lease; otherwise IP, PrefixLen and
// Gateway apply. DNS is advisory and may be empty.
type AddressDesired struct {
	DHCP      bool
	IP        string
	PrefixLen int
	Gateway   string
	DNS       []string
}

// AddressReporter is an optional capability of a Collector: the device's
// own address as the driver knows it now -- the address it manages the
// device through (sonic: an applied IP Settings change, a fallback to
// another known address), or the address the device itself carries when the
// bridge dials it elsewhere (podman: the host's LAN address, dialled over a
// podman bridge network). The loop reports it to the controller as the
// device's address when it is an IP literal: at start unless the operator
// set one (`ip:`), and whenever it changes; "" = unknown.
type AddressReporter interface {
	CurrentAddress() string
}

// ApplyBudget is optional: a driver whose writes take long (SONiC: ~250 ms
// per CLI call, dozens per push) asks for this much time per write --
// apply, SSH keys, address, port or outlet cycle, reboot. Without it a
// write gets the collect budget; the operator's control.apply_timeout
// overrides both.
type ApplyBudget interface {
	ApplyTimeout() time.Duration
}

// PowerEventSource is optional: a power device with no event log of its
// own (no management card, so no syslog) asks the loop to log its power
// transitions -- on battery, AC restored, low battery, overload, output
// off, self-test results, losing contact with it -- as one power-event
// line each (internal/powerevent). A device whose card already logs its
// own events does not implement it, so nothing is logged twice.
type PowerEventSource interface {
	PowerEvents() PowerEventSettings
}

// PowerEventSettings are a device's thresholds for its power-event lines.
// Zero values take the tracker's defaults.
type PowerEventSettings struct {
	// LowRuntime: runtime remaining below this is a low battery even
	// when the device does not signal one itself.
	LowRuntime time.Duration
	// LoadAlarmW: output power at or above this is an overload even
	// when the device does not signal one itself; 0 = the device's rating.
	LoadAlarmW float64
}

// AddressController applies the controller's IP Settings to the device
// itself, so the controller owns the device's address the way it owns
// everything else. Optional. Implementations must be idempotent and must
// refuse an address they cannot verify is reachable from the bridge, because
// a wrong one takes the device off the network.
type AddressController interface {
	ApplyAddress(ctx context.Context, desired AddressDesired) (changed bool, err error)
}

// ManagementIPv6Controller is optional: a device whose management
// interface takes IPv6 from the gateway's router advertisements only when
// told to -- a router (a SONiC switch forwards on every interface, and
// Linux ignores RAs on a forwarding interface unless asked). enabled is
// whether the controller runs IPv6 on the management network; the loop
// asks only a driver that implements this. Idempotent; changed counts the
// writes.
type ManagementIPv6Controller interface {
	ApplyManagementIPv6(ctx context.Context, enabled bool) (changed int, err error)
}

// FanMode is a device's fan behaviour as UniFi names it: Default (the
// vendor's own fan control) or Quiet (a lower fan floor or a fixed lower
// speed; each driver says what it writes).
type FanMode string

const (
	FanModeDefault FanMode = "default"
	FanModeQuiet   FanMode = "quiet"
)

// ParseFanMode reads a fan mode name ("default" or "quiet", any case).
func ParseFanMode(s string) (FanMode, bool) {
	switch m := FanMode(strings.ToLower(strings.TrimSpace(s))); m {
	case FanModeDefault, FanModeQuiet:
		return m, true
	}
	return "", false
}

// FanModeController sets a device's fan mode. The loop calls it from a
// collect whose System.FanMode differs from the mode wanted (the device's
// fan_mode setting) and System.FanModes holds that mode, so a converged
// device is never written; the driver reads the mode back on its next
// collect. Optional; a driver that cannot set the mode on the device in
// hand leaves System.FanModes empty and logs why at Start.
type FanModeController interface {
	ApplyFanMode(ctx context.Context, mode FanMode) error
}

// SyslogController gives the controller's remote syslog host to a device
// without port control -- a power device whose management card keeps its own
// list of syslog servers -- so the controller owns where it logs, as it does
// for a switch (which takes the host with the rest of DeviceDesired). hosts
// is the push's list, "ip" or "ip:port" with 514 left out; nil means remote
// logging is off in UniFi, and the device stops sending. Optional.
// Implementations must be idempotent: a device that already logs to exactly
// hosts is not written.
type SyslogController interface {
	ApplySyslog(ctx context.Context, hosts []string) (changed bool, err error)
}

// OutletController switches and names outlets on a power device. It is
// optional and discovered with a type assertion, like the other write-side
// interfaces, so a read-only PDU driver is a valid first step.
//
// Implementations must be idempotent: compare each desired value with the
// device's current state and write only the differences. Switching an outlet
// is a real relay on real load, so a redundant write is not free.
type OutletController interface {
	ApplyOutlets(ctx context.Context, desired []OutletDesired) (changed int, err error)
}

// OutletPlanner is an optional OutletController capability: PlanOutlets
// reports which outlets ApplyOutlets would switch for desired, without
// writing. The loop uses it to hold the first outlet push it sees in a run
// when that push would switch something -- an outlet is real load, and a
// controller that disagrees with the device on first contact is more likely
// stale than right.
type OutletPlanner interface {
	PlanOutlets(desired []OutletDesired) (changed []int)
}

// Controller implementations also need the site VLAN list to exist on the
// switch before ports can reference it.
type VLANController interface {
	EnsureVLANs(ctx context.Context, ids []int) (created int, err error)
}

// Snapshot is the state of one device at one instant.
type Snapshot struct {
	TakenAt  time.Time
	System   System
	Ports    []Port     // sorted by Index, one entry per front-panel port
	MACTable []MACEntry // every learned address, including those on non-front-panel ports
	VLANs    []int      // VLAN IDs that exist on the switch, ascending

	// Outlets is the power-device shape, the way Ports is the switch shape:
	// a rack PDU fills this and leaves Ports holding only its uplink. Sorted
	// by Index. Empty for a device that has no outlets.
	Outlets []Outlet

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
		if p.Neighbor == nil || !p.Neighbor.IsBridge || p.Neighbor.Child || !p.Up {
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

// System is the device's identity and health.
type System struct {
	Vendor  string
	Model   string
	SKU     string // the vendor's order code when the device reports one apart from Model (an APC unit: "Smart-UPS 1500" / SMTL1500RM3UC)
	Serial  string
	MAC     string // primary/system MAC, lower-case colon form
	MgmtMAC string // the out-of-band management interface's own MAC, "" if none/unknown (UniFi's "service" interface)

	// OOBInterfaces lists dedicated out-of-band management interfaces. A
	// UniFi controller expects a switch's management address in-band, behind
	// its uplink; an address on an OOB port leaves the switch without a place
	// in the topology (see docs/adding-a-device.md). Reported so the loop can
	// warn loudly.
	OOBInterfaces []OOBInterface

	LoadAvg          []float64 // 1/5/15-minute load averages, nil if unknown
	MACTableCapacity int       // hardware MAC (FDB) table size, 0 if unknown
	MACTableUsed     int       // entries in use

	Addresses  []IfAddress       // the switch's own IPv4 addresses (management, VLAN, loopback)
	IPv6       []string          // the management interface's IPv6 addresses with their prefix ("fe80::1/64"), global first; nil if none or unknown
	ARP        map[string]string // IPv4 -> MAC the switch has resolved, lower-case colon form
	GatewayMAC string            // MAC of the switch's gateway/controller next hop, "" if unknown
	Gateway    string            // the device's own default gateway address, "" if unknown
	DHCP       bool              // the device takes its management address from DHCP (vs a manual one)
	Version    string            // vendor's own version string, e.g. "4.26.14M"
	Hostname   string
	Timezone   string // the device's configured time zone name, "" if unknown
	Uptime     time.Duration

	CPUPercent  float64 // 0-100, whole system
	MemTotalKB  uint64
	MemUsedKB   uint64
	MemBufferKB uint64

	TemperatureC   float64
	HasTemperature bool
	Overheating    bool

	Fans []Fan
	PSUs []PSU

	// FanModes are the fan modes the driver can set on this device (nil =
	// it cannot set any); FanMode is the one the device runs now, read back
	// from the device each collect ("" = unknown). See FanModeController.
	FanModes []FanMode
	FanMode  FanMode

	// Aggregate power for a device that measures its whole load rather than
	// each outlet (a switched rack PDU meters the phase, not the outlet).
	// HasPowerDraw distinguishes "measured zero" from "does not measure":
	// a device with no sensor must not report 0 W, which reads as a real
	// measurement of an idle rack.
	PowerBudgetW  float64
	PowerDrawW    float64
	PowerCurrentA float64
	HasPowerDraw  bool

	// Battery is set only by a device that has one (a UPS). Its presence is
	// what makes the presentation layer emit the battery tables; a switch or
	// a PDU leaves it nil. Everything in it is a measurement or a derived
	// state, never a claim about hardware the driver cannot see.
	Battery *Battery

	STPMode     string // "rstp", "mstp", "stp", "none", ""
	STPPriority int    // bridge priority, 0 if unknown
	STPRoot     string // MAC of the root bridge (this switch's own when it is the root), "" if unknown
	// STPTopologyChanges is the bridge's own topology-change count and
	// STPLastTopologyChange when the last one happened, where the device
	// keeps them (zero = not known; the presentation layer then sums the
	// ports' changes).
	STPTopologyChanges    int
	STPLastTopologyChange time.Time

	IGMPSnooping    map[int]bool // VLAN ID -> snooping enabled
	NTPServers      []string
	SyslogHosts     []string
	DHCPSnooping    bool
	SNMPCommunities []string // configured v1/v2c communities
}

// IfAddress is one of the switch's own IPv4 addresses.
type IfAddress struct {
	Iface     string
	IP        string
	PrefixLen int
}

// OOBInterface is one out-of-band management interface.
type OOBInterface struct {
	Name string
	Up   bool   // link up (cabled and active)
	IP   string // configured IPv4 address, "" if none
	// AdminDown: the interface is administratively down (taken down on
	// purpose), so its address answers nowhere until it is brought up.
	// False when the driver cannot tell.
	AdminDown bool
}

// PortVLAN is a port's configured 802.1Q state.
type PortVLAN struct {
	Mode       string // "trunk" | "access" | ""
	NativeVLAN int    // trunk native / access VLAN
	AllowAll   bool   // trunk carries every VLAN
	Allowed    []int  // trunk allowed VLANs when !AllowAll (sorted, includes native if allowed)
}

// Fan is one fan's state: a fan module (drawer) where the device has them.
type Fan struct {
	Label     string
	SpeedPct  int
	TargetPct int // the speed the device's fan control asks for; 0 = not reported (SpeedPct stands in)
	OK        bool
	Missing   bool   // the slot is empty (a module pulled); false when present or not known
	Direction string // airflow as the device names it: "intake" (port side in) or "exhaust"; "" if unknown
}

// PSU is one power supply's state.
type PSU struct {
	Slot      string
	Model     string
	Serial    string
	Present   bool // a supply is fitted in the slot
	OK        bool // present and delivering power
	HotSwap   bool // the supply can be replaced while the device runs
	OutputW   float64
	InputW    float64 // power drawn from the feed; 0 if the supply does not report it
	CapacityW float64
	TempsC    []float64 // the supply's own temperature sensors, if any
	FanRPM    []int     // the supply's own fans, if it reports their speed in RPM
}

// MACEntry is one learned MAC address. The two times are what a UniFi
// switch reports as an entry's `uptime` and `age` (docs/mac-table-times.md):
// the controller takes a wired client's connected-since from the learn
// time, so it must not be refreshed by traffic.
type MACEntry struct {
	MAC       string
	VLAN      int
	PortIndex int // front-panel port it was learned on; 0 = CPU/other
	// LearnedAt is when the address was learned on this port, or last moved
	// to it; zero = the driver cannot know (the loop's learn-time tracker
	// then supplies one).
	LearnedAt time.Time
	// LastSeen is when a frame from the address was last seen; zero =
	// unknown.
	LastSeen time.Time
}

// Optic describes an installed transceiver and its DOM readings.
type Optic struct {
	Vendor     string
	Part       string
	Serial     string
	MediaType  string  // vendor string, e.g. "100GBASE-CWDM4"
	Revision   string  // the module's hardware revision, "" if unknown
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

	// Settings a driver reads back where it applies them (nil/zero = the
	// switch's default: not isolated, no MAC restriction, no egress limit,
	// LLDP-MED on).
	Isolated     bool
	PortSecurity bool
	AllowedMACs  []string
	EgressKbps   int
	LLDPMEDOff   bool
	VoiceVLAN    int
	// Dot1X is the port's 802.1X control mode as applied ("" = none), and
	// Dot1XAuthorized the hosts it admitted (lower-case MACs).
	Dot1X           Dot1XMode
	Dot1XAuthorized []string

	Present bool // an optic/cable is present (always true for copper)
	Enabled bool // administratively up
	// Fault is the vendor fault state keeping the port down, e.g.
	// "errdisabled: speed-misconfigured"; "" = none. When the device's own
	// protection shut the port down, the text carries the cause as one of:
	// storm-broadcast, storm-multicast, storm-unicast (a traffic storm of
	// that type), dhcp-rate-limit (too many DHCP frames), mac-locking (a
	// source address not allowed on the port), dot1x (failed port
	// authentication), denial-of-service, loop-protect, bpduguard.
	Fault      string
	Up         bool // link up
	SpeedMbps  int
	FullDuplex bool
	MTU        int

	Counters Counters
	STPState string // "forwarding", "blocking", "learning", "listening", "disabled", ""
	STPRole  string // "root", "designated", "alternate", "backup", "disabled", ""

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
	LACP         *LACP  // the member's LACP state; nil if not a member or not readable
	MirrorFrom   int    // >0: configured as a mirror destination for that source port
	FEC          FEC    // operating
	FECConfig    FEC    // configured (from running-config); "" = not configured
	BPDUGuard    bool
	STPEdge      bool              // portfast + bpdufilter configured
	STPOperEdge  bool              // the port runs as an edge port now (configured, or detected by auto edge)
	StormCtrl    *StormControlSpec // nil = none configured
	Optic        *Optic            // nil for copper or an empty cage
	MACs         []MACEntry        // addresses learned on this port
	Health       PortHealth        // fault and change signals (anomaly reporting)
}

// LACP is an aggregate member's LACP state as the device's LACP daemon
// holds it.
type LACP struct {
	// Active: the member is bundled -- selected for its aggregate and in
	// sync with the partner, so it carries the aggregate's traffic.
	Active bool
	// PartnerSystem is the partner's LACP system id (a MAC, lower-case
	// colon form), "" when the driver cannot read it or no partner answers.
	PartnerSystem string
}

// Outlet is one outlet on a power device. Outlets are to a PDU what ports are
// to a switch: the structural table the controller renders its UI from. Index
// is 1-based and is the position the controller addresses in its outlet
// overrides, so it must match the device's own numbering rather than a
// position in this slice.
type Outlet struct {
	Index int
	Name  string // the device's own label for the outlet
	On    bool   // the relay is closed (delivering power)

	// Switchable is false for an outlet that is permanently live (an unswitched
	// bank on a hybrid PDU). The presentation layer will not offer a control
	// the driver cannot honour.
	Switchable bool

	// HasMetering says the per-outlet measurements below are real. A device
	// that does not meter leaves it false: reporting zeros would read as a
	// genuine measurement of no load.
	HasMetering bool
	VoltageV    float64
	CurrentA    float64
	PowerW      float64
}

// Battery is the state of a UPS: its pack, its load and where its power is
// coming from. Values a device does not report are left at zero with the
// matching Has* flag false, so a consumer never mistakes "unmeasured" for
// "measured zero" (an input voltage of 0 V reads as a real outage).
type Battery struct {
	OnBattery   bool // the load is being carried by the battery (mains lost)
	Charging    bool // the pack is being charged
	LowBattery  bool // the device signals shutdown-imminent
	Overload    bool // the load exceeds the device's rating
	OutputOff   bool // the output is switched off
	Bypass      bool // the load is on bypass, not conditioned output
	Testing     bool // a self-test is running
	Calibrating bool // a runtime calibration is running
	ECOMode     bool // high-efficiency / ECO mode
	Boost       bool // AVR is boosting a low line
	Trim        bool // AVR is trimming a high line
	Fault       bool // the device reports a general fault
	InputBad    bool // the device reports its input as not acceptable
	LowRuntime  bool // runtime remaining is below the device's own low-runtime warning threshold

	// TestResult is the outcome of the last self-test as the device holds
	// it: "passed", "failed", "refused", "aborted", "in_progress",
	// "pending", or "" when it records none.
	TestResult string

	ChargePct int           // state of charge, 0-100
	Runtime   time.Duration // estimated runtime remaining at the present load
	VoltageV  float64       // pack voltage

	TemperatureC   float64 // pack temperature
	HasTemperature bool

	LoadPct           float64 // output load as a percentage of the real-power rating
	RealPowerW        float64 // output real power
	ApparentPowerVA   float64 // output apparent power
	RealPowerRatingW  float64 // nameplate real-power rating
	ApparentRatingVA  float64 // nameplate apparent-power rating
	OutputVoltageV    float64
	OutputCurrentA    float64
	OutputFrequencyHz float64
	HasOutput         bool // the output measurements above are real

	InputVoltageV float64
	HasInput      bool // false when the device reports the input as not applicable

	EfficiencyPct float64
	HasEfficiency bool // false when the device gives a reason instead of a number

	TransferHighV int // AVR/transfer thresholds, volts; 0 if unknown
	TransferLowV  int

	ShutdownDelay time.Duration // configured delay before a commanded shutdown
	TransferCause string        // the device's reason for the last status change, "" if unknown
}

// PortHealth carries the vendor-neutral fault and change signals the
// presentation layer reports per port (link drops, STP state changes, optic
// fault flags, FEC counters for the satisfaction score). Counters are
// lifetime values.
type PortHealth struct {
	LinkChanges    uint64 // link state transitions, counted from a link that started down: LinkChanges/2 is the drop count (link_down_count), so a driver whose device counts otherwise converts
	STPChanges     int    // spanning-tree state changes seen on this port; never falls while the bridge runs (a device that restarts the count per link-up is held by its driver: arista-eos)
	OpticRxAlarm   bool   // receive fault: the module's loss of signal (sonic), or a receive power outside its alarm thresholds (arista-eos)
	OpticTxAlarm   bool   // transmit fault: the module's own flag (sonic), or a transmit power or laser bias outside the alarm thresholds (arista-eos)
	HasFECCounters bool   // FECCorrected/FECUncorrected are populated
	FECCorrected   uint64 // FEC corrected codewords
	FECUncorrected uint64 // FEC uncorrected codewords (each one is a lost frame)

	// Frames this port's own protection dropped, cumulative since the
	// device booted. Zero means not reported (or none yet); the consumer
	// keeps the previous sample and looks at growth only.
	StormDropsBroadcast uint64 // broadcast frames dropped by the port's storm control
	StormDropsMulticast uint64 // multicast frames dropped by the port's storm control
	StormDropsUnicast   uint64 // unicast frames dropped by the port's storm control (unknown-unicast flood)
	DHCPRateDrops       uint64 // DHCP frames dropped by the port's DHCP rate limit
	RogueDHCPDrops      uint64 // DHCP server replies dropped on this port by DHCP guarding (a server on an untrusted port)
	MACLockingDrops     uint64 // frames from a source not allowed on the port that the port's MAC filter dropped
	// Dot1XFailures is the port's failed 802.1X authentication attempts
	// (the RADIUS servers refused the host), cumulative since boot, read
	// like the drop counters above.
	Dot1XFailures uint64
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
	// Child: the bridge filled this in for a device it reports below the
	// port (a declared child, informloop), not one the device learned. A
	// child is never this device's uplink.
	Child bool
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
	STP         bool // switch-wide STP mode/priority
	BPDUGuard   bool // per-port BPDU guard
	STPAutoEdge bool // per-port edge auto/enabled/disabled (PortDesired.STPEdgeMode)
	STPPortCost bool // per-port STP path cost is reported
	// LoopProtection: the driver honours PortDesired.LoopProtect and takes a
	// looping port down by itself (reported as a port Fault containing
	// "loop-protect").
	LoopProtection bool
	Jumbo          bool // jumbo frames (reported and/or switchable)
	FEC            bool // per-port forward error correction
	LACP           bool // link aggregation (Controller honours PortDesired.LAG)
	StormControl   bool // per-port storm control in percent
	IGMPSnooping   bool // IGMP snooping (DeviceController honours IGMPSnooping)
	// IGMPSnoopingAdvanced: the per-VLAN querier, timers and
	// unknown-multicast handling and the reserved groups (DeviceDesired.IGMP,
	// IGMPFloodReserved), as UniFi's own advanced-snooping switches take them.
	IGMPSnoopingAdvanced bool
	LLDPMED              bool
	DHCPSnooping         bool // DeviceController honours DHCPSnooping and DHCPGuards
	PortIsolation        bool // PortDesired.Isolation
	// PortSecurity: the per-port MAC allow list (PortDesired.PortSecurity,
	// AllowedMACs) is honoured.
	PortSecurity bool
	// EgressRateLimit: PortDesired.EgressKbps is honoured.
	EgressRateLimit bool
	// Dot1X: 802.1X port authentication (DeviceDesired.Dot1X and each
	// PortDesired.Dot1X) is honoured.
	Dot1X bool
	// L3Routing: VLAN interfaces and static routes owned by the controller
	// (DeviceDesired.L3Interfaces / StaticRoutes); L3Interfaces and
	// StaticRoutes are the counts the controller is told.
	L3Routing    bool
	L3Interfaces int
	StaticRoutes int
	// BGP: the controller's BGP router for this switch (an uploaded FRR
	// configuration, DeviceDesired.BGP) is honoured.
	BGP bool
	// OSPF: the controller's OSPF router for this switch
	// (DeviceDesired.OSPF) is honoured.
	OSPF bool
	// PortRouting: the port editor's Routing and Routing + Aggregating
	// operation modes (a port or an aggregate with an address of its own,
	// in no VLAN) are honoured.
	PortRouting bool
	SNMP        bool // DeviceController honours SNMPCommunity
	// IPACLs / MACACLs are how many switch ACL rules of each kind the
	// DeviceController honours (DeviceDesired.IPACLs / MACACLs); 0 = none.
	IPACLs  int
	MACACLs int
	// MirrorSessions / AggregateSessions are the counts the UI is told; 0
	// hides the feature.
	MirrorSessions    int
	AggregateSessions int
}

// Capable is implemented by a Device that wants to declare its own
// Capabilities; a Device without it gets the presentation layer's default
// (the Arista EOS set).
type Capable interface {
	Capabilities() Capabilities
}

// Planner is an optional Controller capability: PlanPorts reports which
// ports ApplyPorts would change for desired, without writing anything. The
// loop uses it to hold a freshly adopted device's first push when it would
// alter ports — the controller's default config for a new device is "every
// port at its defaults", which would strip whatever the switch had.
type Planner interface {
	PlanPorts(desired []PortDesired) (changed []int)
}
