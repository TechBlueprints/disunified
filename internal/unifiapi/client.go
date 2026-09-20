// Package unifiapi is the small slice of the controller's REST API the bridge
// uses for things the inform protocol cannot express: the device's display
// name and per-port names, which live in the controller's configuration.
// Authenticated with a UniFi API key (X-API-KEY), the same one home-setup
// scripts use; works through the UniFi OS proxy (/proxy/network/...).
package unifiapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one controller and site.
type Client struct {
	BaseURL string // https://unifi.example.net
	APIKey  string
	Site    string
	http    *http.Client
}

// New builds a client. insecure skips TLS verification (self-signed).
func New(baseURL, apiKey, site string, insecure bool) *Client {
	if site == "" {
		site = "default"
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // controller self-signed cert
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Site: site, http: &http.Client{Transport: tr, Timeout: 20 * time.Second}}
}

// Device is the subset of a stat/device record the bridge reads.
type Device struct {
	ID            string           `json:"_id"`
	MAC           string           `json:"mac"`
	Name          string           `json:"name"`
	Model         string           `json:"model"`
	PortOverrides []map[string]any `json:"port_overrides"`
	PortTable     []struct {
		PortIdx int    `json:"port_idx"`
		Name    string `json:"name"`
	} `json:"port_table"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-KEY", c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d: %.200s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// DeviceByMAC finds the site's device with mac (lower-case colon form).
func (c *Client) DeviceByMAC(ctx context.Context, mac string) (*Device, error) {
	var r struct {
		Data []Device `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/proxy/network/api/s/"+c.Site+"/stat/device", nil, &r); err != nil {
		return nil, err
	}
	for i := range r.Data {
		if strings.EqualFold(r.Data[i].MAC, mac) {
			return &r.Data[i], nil
		}
	}
	return nil, fmt.Errorf("device %s not found on site %s", mac, c.Site)
}

// UpdateDevice PUTs fields onto the device record (rest/device/<id>); the
// controller merges top-level fields but replaces port_overrides wholesale,
// so callers pass the complete list.
func (c *Client) UpdateDevice(ctx context.Context, id string, fields map[string]any) error {
	return c.do(ctx, http.MethodPut, "/proxy/network/api/s/"+c.Site+"/rest/device/"+id, fields, nil)
}
