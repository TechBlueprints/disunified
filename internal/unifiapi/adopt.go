package unifiapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Adopting one pending device (Network 11.0.99, 2026-11-17;
// docs/drivers/breakout.md 16.12):
//
//   - stat/device lists a device that informs on the default key and has
//     not been adopted with "adopted": false, "state": 2 and no "_id"
//     (controller-11.0.99/stat-device-lane-switch-pending.json: an
//     automatic lane switch beside its adopted parent).
//   - POST cmd/devmgr {"cmd": "adopt", "mac": <mac>} is the UI's Adopt for
//     that one device: its next inform is answered with the mgmt_cfg that
//     carries its key. The POST's own "rc": "ok" proves nothing
//     (cmd/devmgr answers ok to any command name); the proof is the
//     handshake reaching the device.
//
// There is no call here that adopts more than the one MAC it is given, and
// there must never be one: a site's pending list also holds devices that
// are not the site's (a neighbour's access point).

// ErrNotListed: the controller does not list the device at all (it has not
// informed yet, or not for a while).
var ErrNotListed = errors.New("the controller does not list this device")

// ErrNotPending: the controller lists the device, but not as one that is
// waiting to be adopted (it holds a record for it, or shows it in another
// state): nothing is asked.
var ErrNotPending = errors.New("the controller does not list this device as pending adoption")

// AdoptPending asks the controller to adopt the device with exactly this
// MAC, and only when the controller lists it as pending (no record id, not
// adopted). A nil error means only that the controller took the request.
func (c *Client) AdoptPending(ctx context.Context, mac string) error {
	mac = strings.ToLower(strings.TrimSpace(mac))
	var r struct {
		Data []struct {
			ID      string `json:"_id"`
			MAC     string `json:"mac"`
			Adopted bool   `json:"adopted"`
			State   int    `json:"state"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/proxy/network/api/s/"+c.Site+"/stat/device", nil, &r); err != nil {
		return err
	}
	for _, d := range r.Data {
		if !strings.EqualFold(d.MAC, mac) {
			continue
		}
		if d.ID != "" || d.Adopted {
			return fmt.Errorf("%w (adopted %v, state %d)", ErrNotPending, d.Adopted, d.State)
		}
		var a struct {
			Meta struct {
				RC  string `json:"rc"`
				Msg string `json:"msg"`
			} `json:"meta"`
		}
		if err := c.do(ctx, http.MethodPost, "/proxy/network/api/s/"+c.Site+"/cmd/devmgr", map[string]any{"cmd": "adopt", "mac": mac}, &a); err != nil {
			return err
		}
		if a.Meta.RC != "" && a.Meta.RC != "ok" {
			return fmt.Errorf("cmd/devmgr adopt: %s %s", a.Meta.RC, a.Meta.Msg)
		}
		return nil
	}
	return ErrNotListed
}
