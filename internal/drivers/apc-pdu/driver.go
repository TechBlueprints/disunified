// Package apcpdu presents an APC rack PDU as an adopted UniFi power
// distribution unit: each of the card's switched outlets becomes an outlet
// the controller can name and switch.
//
// Written against an AP7931 on a Network Management Card running AOS 3.9.2
// (hardware revision B2, 2008); fixtures in docs/fixtures/apc-aos-3.9.2.
// The card has no API -- /api and /rest are 404 and its web UI is form posts
// that sniff for MSIE 5.5 -- so SNMPv1 is the vendor's own control plane and
// the one this driver uses, with the card's config file over FTP for the one
// thing SNMP cannot write. See docs/drivers/apc-pdu.md.
package apcpdu

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Driver is the APC rack PDU driver.
//
// Config: url = the card's address (host or host:161; a snmp:// or http://
// prefix is accepted and ignored). username/password are the card's own
// admin login, used for the FTP config upload that renames outlets; without
// them the driver still reads and switches, and refuses to rename. Options:
//
//	read_community   SNMPv1 community for reads (default "public")
//	write_community  SNMPv1 community for outlet switching (default "private").
//	                 The card's access control entry for it must have access
//	                 type Write+, not Write: with Write it drops every SET
//	                 silently, with no error and no log entry.
//	outlets          present only the first N outlets (default: every outlet
//	                 the card reports)
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "apc-pdu" }

func (Driver) Describe() string {
	return "APC rack PDU over SNMPv1 (outlets read and switched) plus FTP config.ini for outlet names; verified on AP7931 / AOS 3.9.2"
}

// Open builds the SNMP/FTP runner and the collector; Start then reads the
// whole card once, so a wrong community or an unreachable card fails here.
func (Driver) Open(ctx context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
	host := strings.TrimSpace(cfg.URL)
	if host == "" {
		host = cfg.Options["host"]
	}
	if host == "" {
		return nil, errors.New("apc-pdu: set url: the PDU's address")
	}
	host = stripScheme(host)
	if host == "" {
		return nil, fmt.Errorf("apc-pdu: could not read a host out of url %q", cfg.URL)
	}

	r := NewSNMP(host)
	r.User, r.Password = cfg.Username, cfg.Password
	if v := cfg.Options["read_community"]; v != "" {
		r.ReadCommunity = v
	}
	if v := cfg.Options["write_community"]; v != "" {
		r.WriteCommunity = v
	}
	if v := cfg.Options["timeout"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("apc-pdu: timeout %q: %w", v, err)
		}
		r.Timeout = d
	}

	c := NewCollector(r)
	if v := cfg.Options["outlets"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("apc-pdu: outlets must be a number >= 1, got %q", v)
		}
		c.Outlets = n
	}
	return c, nil
}

// stripScheme accepts the address with or without a scheme and port, because
// an operator who has just configured an eAPI switch will write a URL here
// out of habit.
func stripScheme(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimSuffix(s, "/")
	if h, _, ok := strings.Cut(s, "/"); ok {
		s = h
	}
	// A port is not carried: SNMP is always 161 and FTP always 21 on this card.
	if h, p, ok := strings.Cut(s, ":"); ok {
		if _, err := strconv.Atoi(p); err == nil {
			s = h
		}
	}
	return s
}

// Compile-time checks: the PDU is a read/collect device that controls outlets
// and nothing else. It deliberately does not implement devicemodel.Controller
// (it has no switch ports to configure).
var (
	_ devicemodel.Driver           = Driver{}
	_ devicemodel.Device           = (*Collector)(nil)
	_ devicemodel.OutletController = (*Collector)(nil)
	_ devicemodel.OutletCycler     = (*Collector)(nil)
	_ devicemodel.Capable          = (*Collector)(nil)
)
