package switchmodel

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Driver is one vendor/OS implementation, registered by name. A driver
// knows how to reach a switch and turn it into a Switch; everything the
// UniFi side needs is expressed through the neutral types in this package.
//
// Adding a switch means adding a driver: see docs/adding-a-switch.md and
// internal/drivers/CLAUDE.md. The Arista EOS driver is the reference.
type Driver interface {
	// Name is the registry key and the -driver flag value, e.g. "arista-eos".
	Name() string
	// Describe is a one-line summary for -list-drivers.
	Describe() string
	// Open connects to the switch. It must validate the target (OS version,
	// command availability, credentials) and fail loudly rather than return
	// a Switch that would report an empty port table.
	Open(ctx context.Context, cfg DriverConfig) (Switch, error)
}

// DriverConfig is how the operator points a driver at a switch. Drivers
// pick the fields they understand and document them.
type DriverConfig struct {
	URL      string // API endpoint, e.g. https://192.0.2.3/command-api
	SSH      string // user@host[:port] for a CLI transport
	Username string
	Password string
	Options  map[string]string // driver-specific knobs (documented per driver)
}

// Switch is an open connection to one switch. Collect is mandatory; the
// write-side interfaces (Controller, SwitchController, VLANController) are
// optional and discovered with type assertions, so a read-only driver is
// a valid first step.
type Switch interface {
	Collector
	// Start runs every command the driver will ever use once, so a missing
	// command or bad credential fails here, and returns the first snapshot.
	Start(ctx context.Context) (*Snapshot, error)
	Close() error
}

var (
	driversMu sync.Mutex
	drivers   = map[string]Driver{}
)

// RegisterDriver adds a driver; drivers call it from init().
func RegisterDriver(d Driver) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if _, dup := drivers[d.Name()]; dup {
		panic("switchmodel: duplicate driver " + d.Name())
	}
	drivers[d.Name()] = d
}

// LookupDriver returns a registered driver.
func LookupDriver(name string) (Driver, error) {
	driversMu.Lock()
	defer driversMu.Unlock()
	d, ok := drivers[name]
	if !ok {
		return nil, fmt.Errorf("unknown driver %q (known: %v)", name, driverNames())
	}
	return d, nil
}

// Drivers lists registered drivers, sorted by name.
func Drivers() []Driver {
	driversMu.Lock()
	defer driversMu.Unlock()
	out := make([]Driver, 0, len(drivers))
	for _, n := range driverNames() {
		out = append(out, drivers[n])
	}
	return out
}

func driverNames() []string {
	names := make([]string, 0, len(drivers))
	for n := range drivers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
