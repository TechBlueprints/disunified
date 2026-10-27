package sonic

import (
	"math"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Storm control: CONFIG_DB PORT_STORM_CONTROL|<port>|<type> {kbps}. UniFi
// speaks percent of the link, SONiC kbps; UniFi's multicast is SONiC's
// unknown-multicast.
const (
	stormBroadcast = "broadcast"
	stormMulticast = "unknown-multicast"
	stormUnicast   = "unknown-unicast"
)

var stormTypes = []string{stormBroadcast, stormMulticast, stormUnicast}

// stormTable reads PORT_STORM_CONTROL into interface -> type -> kbps.
func stormTable(t table) map[string]map[string]int {
	out := map[string]map[string]int{}
	for k, f := range t {
		parts := strings.Split(k, "|")
		if len(parts) != 3 {
			continue
		}
		if out[parts[1]] == nil {
			out[parts[1]] = map[string]int{}
		}
		out[parts[1]][parts[2]] = atoi(f["kbps"])
	}
	return out
}

// pctToKbps: pct/100 x the port speed (Mb/s) x 1000, rounded.
func pctToKbps(pct float64, speedMbps int) int {
	return int(math.Round(pct / 100 * float64(speedMbps) * 1000))
}

// kbpsToPct reports a kbps limit as a percent of the configured speed, to
// two decimals.
func kbpsToPct(kbps, speedMbps int) float64 {
	if speedMbps <= 0 {
		return 0
	}
	return math.Round(float64(kbps)/float64(speedMbps)/10*100) / 100
}

// stormSpec is a port's storm control in percent; nil when none is set.
func stormSpec(types map[string]int, speedMbps int) *devicemodel.StormControlSpec {
	if len(types) == 0 || speedMbps <= 0 {
		return nil
	}
	set := func(t string) *float64 {
		kbps, ok := types[t]
		if !ok {
			return nil
		}
		v := kbpsToPct(kbps, speedMbps)
		return &v
	}
	return &devicemodel.StormControlSpec{BroadcastPct: set(stormBroadcast), MulticastPct: set(stormMulticast), UnknownUnicastPct: set(stormUnicast)}
}

// stormWant is the kbps per type a desired spec asks for at speedMbps.
func stormWant(spec *devicemodel.StormControlSpec, speedMbps int) map[string]int {
	out := map[string]int{}
	if spec == nil || speedMbps <= 0 {
		return out
	}
	for t, p := range map[string]*float64{stormBroadcast: spec.BroadcastPct, stormMulticast: spec.MulticastPct, stormUnicast: spec.UnknownUnicastPct} {
		if p != nil {
			out[t] = pctToKbps(*p, speedMbps)
		}
	}
	return out
}
