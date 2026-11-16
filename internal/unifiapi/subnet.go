package unifiapi

import (
	"context"
	"fmt"
	"net"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// NetworkFor is the site network whose subnet holds ip: the prefix length
// and the gateway's address, both from the network's ip_subnet, which the
// controller keeps as the gateway's own address with the prefix
// ("198.18.0.1/16" for the Default network in
// docs/fixtures/controller-11.0.99/rest-networkconf-subnets.json). Only
// enabled corporate and guest networks count, which are the ones the gateway
// serves. A VLAN-only network can carry an ip_subnet too (one in the capture
// does), but nothing has shown what that subnet's gateway is, so it is left
// out. When two subnets overlap the longer prefix wins. An error when no
// network holds ip.
func (c *Client) NetworkFor(ctx context.Context, ip string) (devicemodel.SiteNetwork, error) {
	addr := net.ParseIP(ip).To4()
	if addr == nil {
		return devicemodel.SiteNetwork{}, fmt.Errorf("%q is not an IPv4 address", ip)
	}
	var r struct {
		Data []struct {
			Name     string `json:"name"`
			Purpose  string `json:"purpose"`
			Enabled  *bool  `json:"enabled"`
			IPSubnet string `json:"ip_subnet"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/proxy/network/api/s/"+c.Site+"/rest/networkconf", nil, &r); err != nil {
		return devicemodel.SiteNetwork{}, err
	}
	var best devicemodel.SiteNetwork
	for _, n := range r.Data {
		if (n.Purpose != "corporate" && n.Purpose != "guest") || (n.Enabled != nil && !*n.Enabled) || n.IPSubnet == "" {
			continue
		}
		gw, subnet, err := net.ParseCIDR(n.IPSubnet)
		if err != nil || gw.To4() == nil || !subnet.Contains(addr) {
			continue
		}
		if ones, _ := subnet.Mask.Size(); ones > best.PrefixLen {
			best = devicemodel.SiteNetwork{PrefixLen: ones, Gateway: gw.To4().String()}
		}
	}
	if best.PrefixLen == 0 {
		return best, fmt.Errorf("no network on site %s holds %s", c.Site, ip)
	}
	return best, nil
}
