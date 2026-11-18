package unifiapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Forgetting one lane switch (Network 11.0.99; docs/drivers/breakout.md
// 16.13): the controller's own Forget, POST cmd/sitemgr
// {"cmd": "delete-device", "mac": <mac>}, the call the earlier live runs
// made by hand (16.11, 16.12).
//
//   - Its answer carries the whole stored record, the device's x_authkey
//     included. Only meta is ever read from it: the body is not logged,
//     printed or kept, and an error never quotes it.
//   - For about two minutes after an adoption the controller answers
//     api.err.DeviceBusy while the emulated firmware update the lane switch
//     asks for runs (16.12): ErrDeviceBusy, to be asked again later.
//
// It forgets only a device the controller holds a record for (an _id),
// with exactly the MAC given and the lane switch's model: never another
// device, whatever the site holds.

// LaneSwitchModel is the model a lane switch informs as
// (config.LaneModel); a record of any other model is never forgotten here.
const LaneSwitchModel = "USFXG"

// ErrDeviceBusy: the controller refused the Forget because the device is
// busy (api.err.DeviceBusy, the emulated update after an adoption).
var ErrDeviceBusy = errors.New("the controller answered api.err.DeviceBusy (an update is running)")

// ErrNotLaneSwitch: the controller holds a record with this MAC that is not
// a lane switch's (another model); nothing is asked.
var ErrNotLaneSwitch = errors.New("the controller's record with this MAC is not a lane switch")

// ForgetResult is what ForgetLaneSwitch found and did.
type ForgetResult int

const (
	// Forgotten: the controller took the Forget.
	Forgotten ForgetResult = iota + 1
	// NoRecord: the controller holds no record for the MAC (never adopted,
	// already forgotten, or listed only as pending): nothing to forget.
	NoRecord
)

// ForgetLaneSwitch forgets the lane switch with exactly this MAC in the
// controller, when the controller holds an adopted record of the lane
// switch's model for it.
func (c *Client) ForgetLaneSwitch(ctx context.Context, mac string) (ForgetResult, error) {
	mac = strings.ToLower(strings.TrimSpace(mac))
	var r struct {
		Data []struct {
			ID    string `json:"_id"`
			MAC   string `json:"mac"`
			Model string `json:"model"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/proxy/network/api/s/"+c.Site+"/stat/device", nil, &r); err != nil {
		return 0, err
	}
	for _, d := range r.Data {
		if !strings.EqualFold(d.MAC, mac) {
			continue
		}
		if d.ID == "" {
			return NoRecord, nil
		}
		if d.Model != LaneSwitchModel {
			return 0, fmt.Errorf("%w (model %q)", ErrNotLaneSwitch, d.Model)
		}
		return Forgotten, c.deleteDevice(ctx, mac)
	}
	return NoRecord, nil
}

// deleteDevice posts the Forget and reads only the answer's meta.
func (c *Client) deleteDevice(ctx context.Context, mac string) error {
	b, err := json.Marshal(map[string]any{"cmd": "delete-device", "mac": mac})
	if err != nil {
		return err
	}
	path := "/proxy/network/api/s/" + c.Site + "/cmd/sitemgr"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-KEY", c.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var a struct {
		Meta struct {
			RC  string `json:"rc"`
			Msg string `json:"msg"`
		} `json:"meta"`
	}
	_ = json.Unmarshal(raw, &a) // the record in data is never read
	raw = nil
	if a.Meta.Msg == "api.err.DeviceBusy" {
		return ErrDeviceBusy
	}
	if resp.StatusCode/100 != 2 || (a.Meta.RC != "" && a.Meta.RC != "ok") {
		return fmt.Errorf("cmd/sitemgr delete-device: HTTP %d, %s %s", resp.StatusCode, a.Meta.RC, a.Meta.Msg)
	}
	return nil
}
