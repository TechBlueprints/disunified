package unifiapi

import (
	"context"
	"net/http"
	"strings"
)

// The controller's firmware offer for a device, and the request that makes
// the controller send it (Network 11.0.99, live 2026-11-17 on a bridged
// device claimed as a USW Flex XG; docs/feature-map.md, `cmd`s):
//
//   - stat/device/<mac> answers the one record. A connected device the
//     controller has a newer firmware for carries "upgradable": true and
//     "upgrade_to_firmware": "<version>"; one that is up to date carries
//     "upgradable": false and no upgrade_to_firmware
//     (controller-11.0.99/stat-device-upgrade-{offered,taken}.json, the
//     record reduced to the fields read here).
//   - POST cmd/devmgr {"cmd": "upgrade", "mac": <mac>} is what the UI's
//     Update button amounts to: the device's next inform was answered with
//     {"_type": "upgrade", "version": <that version>, "url", "md5sum"}.
//     The POST's own "rc": "ok" proves nothing (cmd/devmgr answers ok to
//     any command name); the proof is that reply reaching the device.

// OfferedUpgrade is the firmware version the controller offers the device
// with mac (lower-case colon form), or "" when it offers none: the device
// is not connected, is up to date, or already reports that version.
func (c *Client) OfferedUpgrade(ctx context.Context, mac string) (string, error) {
	var r struct {
		Data []struct {
			MAC        string `json:"mac"`
			State      int    `json:"state"`
			Version    string `json:"version"`
			Upgradable bool   `json:"upgradable"`
			UpgradeTo  string `json:"upgrade_to_firmware"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/proxy/network/api/s/"+c.Site+"/stat/device/"+mac, nil, &r); err != nil {
		return "", err
	}
	for _, d := range r.Data {
		if !strings.EqualFold(d.MAC, mac) {
			continue
		}
		// State 1 is connected; an update is not asked for a device that
		// is pending, provisioning or already upgrading.
		if d.State != 1 || !d.Upgradable || d.UpgradeTo == "" || d.UpgradeTo == d.Version {
			return "", nil
		}
		return d.UpgradeTo, nil
	}
	return "", nil
}

// RequestUpgrade asks the controller to send the device with mac the
// firmware it offers. A nil error means only that the controller took the
// request.
func (c *Client) RequestUpgrade(ctx context.Context, mac string) error {
	return c.do(ctx, http.MethodPost, "/proxy/network/api/s/"+c.Site+"/cmd/devmgr", map[string]any{"cmd": "upgrade", "mac": mac}, nil)
}
