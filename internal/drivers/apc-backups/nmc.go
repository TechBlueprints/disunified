// Package apcbackups presents an APC Back-UPS Pro network model (the
// "Gassan" family: Back-UPS Pro 500/1000/1500 with the embedded AP9537-class
// Network Management Card, AOS 6) to the controller as a UniFi UPS.
//
// The card exposes nothing about the UPS over SNMP -- its PowerNet tree
// holds only the card's own firmware and ports -- so the driver reads and
// controls the UPS through the card's web pages: the same form-handler
// surface the apc-pdu driver uses for names, and the only machine interface
// there is (no JSON, no XHR, no API; the /Forms/* handlers take what a
// browser would post). Identity, battery, input and per-outlet load come
// from three pages; the two switched outlet groups are controlled through
// the card's two-step On/Off/Reboot form; the card's own TCP/IP settings go
// through a partial config.ini over FTP, as on the PDU.
//
// Verified on a Back-UPS Pro 500 (BG500, UPS 05.3, NMC AOS 6.0.1 / gsn
// 6.0.4), 2026-09-27.
package apcbackups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

// Runner is the card as the driver sees it: pages by name within one
// login, the two-step outlet control, and config.ini over FTP. The live
// Web runner and the fixture runner both implement it.
type Runner interface {
	// Pages logs in, fetches each named page ("home", "uloutcfg2", ...),
	// logs out, and returns the pages' HTML by name.
	Pages(ctx context.Context, names ...string) (map[string]string, error)
	// Control switches outlet groups: actions maps a switched-group number
	// (1-based, as the card counts them) to a control code; groups not in
	// the map get "no action". It walks both steps of the card's form and
	// returns the SogControl value the confirmation carried.
	Control(ctx context.Context, actions map[int]string) (string, error)
	// PutConfig uploads a partial config.ini; the card applies it live.
	PutConfig(ctx context.Context, body []byte) error
	// GetConfig downloads the card's whole config.ini.
	GetConfig(ctx context.Context) ([]byte, error)
}

// Control codes, the option values of the card's On/Off/Reboot selects.
const (
	codeNone   = "00000000"
	codeOn     = "01000000"
	codeOff    = "03000000"
	codeReboot = "05000000"
)

// Web is the live Runner: HTTP to the card's NMC with a session token in
// the path (the card's own scheme), and FTP for config.ini.
type Web struct {
	Base     string // http://host
	Host     string // host, for FTP
	User     string
	Password string
	Timeout  time.Duration
	http     *http.Client
}

// NewWeb builds the runner for a card at base (scheme and host).
func NewWeb(base, user, password string) *Web {
	jar, _ := cookiejar.New(nil)
	host := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
	host = strings.SplitN(host, "/", 2)[0]
	w := &Web{Base: strings.TrimRight(base, "/"), Host: host, User: user, Password: password, Timeout: 15 * time.Second}
	w.http = &http.Client{
		Jar:     jar,
		Timeout: w.Timeout,
		// Redirects are followed by hand: the 303 after a login or a form
		// post carries the session token (login) or the confirmation page
		// (control) in its Location.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return w
}

var tokenRe = regexp.MustCompile(`/NMC/([^/]+)/`)

// login posts the card's login form and returns the session token from
// the redirect it answers with.
func (w *Web) login(ctx context.Context) (string, error) {
	// A fresh cookie jar for every session. The card sets a session cookie
	// at login and requires it on every page, but a login that carries the
	// cookie of a session the card has since dropped is answered 400 -- and
	// a jar kept across polls carried exactly that after the card
	// invalidated one session, so every later login failed (143 x 10 polls
	// on 2026-09-27/28, the controller showing 18 h of stale data).
	if jar, err := cookiejar.New(nil); err == nil {
		w.http.Jar = jar
	}
	form := url.Values{"login_username": {w.User}, "login_password": {w.Password}, "submit": {"Log On"}}
	res, err := w.post(ctx, w.Base+"/Forms/login1", form)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	loc := res.Header.Get("Location")
	m := tokenRe.FindStringSubmatch(loc)
	if res.StatusCode != http.StatusSeeOther || m == nil {
		return "", fmt.Errorf("login: HTTP %d, no session in %q (bad credentials, or the card's session limit is reached)", res.StatusCode, loc)
	}
	return m[1], nil
}

func (w *Web) logout(ctx context.Context, token string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.Base+"/NMC/"+token+"/logout.htm", nil)
	if err != nil {
		return
	}
	if res, err := w.http.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
}

func (w *Web) get(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	res, err := w.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", u, res.StatusCode)
	}
	return string(b), nil
}

func (w *Web) post(ctx context.Context, u string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := w.http.Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res, nil
}

// Pages implements Runner.
func (w *Web) Pages(ctx context.Context, names ...string) (map[string]string, error) {
	token, err := w.login(ctx)
	if err != nil {
		return nil, err
	}
	defer w.logout(ctx, token)
	out := map[string]string{}
	for _, n := range names {
		body, err := w.get(ctx, w.Base+"/NMC/"+token+"/"+n+".htm")
		if err != nil {
			return nil, err
		}
		out[n] = body
	}
	return out, nil
}

var sogControlRe = regexp.MustCompile(`name="SogControl"\s+value="([^"]*)"`)

// Control implements Runner: step one posts the selects and is answered
// with a redirect to the confirmation page, whose hidden SogControl value
// is posted back with Apply. Nothing switches until that second post.
func (w *Web) Control(ctx context.Context, actions map[int]string) (string, error) {
	token, err := w.login(ctx)
	if err != nil {
		return "", err
	}
	defer w.logout(ctx, token)
	form := url.Values{"submit": {"Next ››"}}
	groups := maxSOG
	for n := range actions {
		if n > groups {
			groups = n
		}
	}
	for n := 1; n <= groups; n++ {
		code := actions[n]
		if code == "" {
			code = codeNone
		}
		form.Set(fmt.Sprintf("sog_control?%d", n), code)
	}
	res, err := w.post(ctx, w.Base+"/NMC/"+token+"/Forms/ulsogctl1", form)
	if err != nil {
		return "", fmt.Errorf("outlet control step 1: %w", err)
	}
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "ulsogcfm") {
		return "", fmt.Errorf("outlet control step 1: HTTP %d -> %q, expected the confirmation page", res.StatusCode, loc)
	}
	if strings.HasPrefix(loc, "/") {
		loc = w.Base + loc
	}
	page, err := w.get(ctx, loc)
	if err != nil {
		return "", fmt.Errorf("outlet control confirmation: %w", err)
	}
	m := sogControlRe.FindStringSubmatch(page)
	if m == nil {
		return "", errors.New("outlet control confirmation: no SogControl field on the page")
	}
	res, err = w.post(ctx, w.Base+"/NMC/"+token+"/Forms/ulsogcfm1", url.Values{"SogControl": {m[1]}, "submit": {"Apply"}})
	if err != nil {
		return "", fmt.Errorf("outlet control apply: %w", err)
	}
	if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("outlet control apply: HTTP %d", res.StatusCode)
	}
	return m[1], nil
}

// maxSOG is how many switched groups the form is posted for at least; a
// Back-UPS Pro has two, the confirmation lists four slots.
const maxSOG = 2

// PutConfig implements Runner over FTP, as the PDU driver does.
func (w *Web) PutConfig(ctx context.Context, body []byte) error {
	c, err := ftp.Dial(w.Host+":21", ftp.DialWithContext(ctx), ftp.DialWithTimeout(w.Timeout))
	if err != nil {
		return fmt.Errorf("ftp dial %s: %w", w.Host, err)
	}
	defer func() { _ = c.Quit() }()
	if err := c.Login(w.User, w.Password); err != nil {
		return fmt.Errorf("ftp login: %w", err)
	}
	if err := c.Stor("config.ini", strings.NewReader(string(body))); err != nil {
		return fmt.Errorf("ftp store config.ini: %w", err)
	}
	return nil
}

// GetConfig implements Runner over FTP.
func (w *Web) GetConfig(ctx context.Context) ([]byte, error) {
	c, err := ftp.Dial(w.Host+":21", ftp.DialWithContext(ctx), ftp.DialWithTimeout(w.Timeout))
	if err != nil {
		return nil, fmt.Errorf("ftp dial %s: %w", w.Host, err)
	}
	defer func() { _ = c.Quit() }()
	if err := c.Login(w.User, w.Password); err != nil {
		return nil, fmt.Errorf("ftp login: %w", err)
	}
	r, err := c.Retr("config.ini")
	if err != nil {
		return nil, fmt.Errorf("ftp retrieve config.ini: %w", err)
	}
	defer r.Close()
	return io.ReadAll(r)
}
