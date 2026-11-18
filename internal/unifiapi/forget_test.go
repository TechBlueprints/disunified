package unifiapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The controller's list with an automatic lane switch adopted beside its
// parent (Network 11.0.99, 2026-11-18, the DX010's cage 26 right after its
// adoption; both records reduced): 02:00:00:00:00:1a is the USFXG.
func adoptedLaneList(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "stat-device-lane-switch-adopted.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// forgetServer serves the list and answers each Forget with the next of
// answers (status and meta as the live run recorded them,
// controller-11.0.99/forget-lane-switch.json), the record in data standing
// for the stored one the controller returns (its x_authkey a placeholder).
func forgetServer(t *testing.T, list []byte, answers ...forgetCapture) (*Client, *[]map[string]any) {
	t.Helper()
	var posts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/s/default/stat/device"):
			w.Write(list)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/s/default/cmd/sitemgr"):
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			posts = append(posts, m)
			a := answers[0]
			if len(answers) > 1 {
				answers = answers[1:]
			}
			body := map[string]any{"meta": a.Meta}
			if a.Records > 0 {
				body["data"] = []map[string]any{{"_id": "00000000000000000000000000000002", "mac": m["mac"], "x_authkey": "00000000000000000000000000000001"}}
			}
			out, _ := json.Marshal(body)
			w.WriteHeader(a.Status)
			w.Write(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-key", "", false), &posts
}

// forgetCapture is one recorded answer to a Forget.
type forgetCapture struct {
	State   int               `json:"state"` // the record's state when it was asked
	Status  int               `json:"status"`
	Meta    map[string]string `json:"meta"`
	Records int               `json:"data_records"`
}

func capturedForgets(t *testing.T) (ok, busy forgetCapture) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "forget-lane-switch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Answers []forgetCapture `json:"answers"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.Answers {
		switch a.Meta["msg"] {
		case "":
			ok = a
		case "api.err.DeviceBusy":
			busy = a
		}
	}
	if ok.Status == 0 || busy.Status == 0 {
		t.Fatalf("the capture lacks an ok or a busy answer: %+v", f.Answers)
	}
	return ok, busy
}

func TestForgetLaneSwitchForgetsExactlyThatRecord(t *testing.T) {
	ok, busy := capturedForgets(t)
	c, posts := forgetServer(t, adoptedLaneList(t), busy, ok)
	ctx := context.Background()

	_, err := c.ForgetLaneSwitch(ctx, "02:00:00:00:00:1A")
	if !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("busy: %v, want ErrDeviceBusy", err)
	}
	res, err := c.ForgetLaneSwitch(ctx, "02:00:00:00:00:1a")
	if err != nil || res != Forgotten {
		t.Fatalf("ForgetLaneSwitch = %v, %v", res, err)
	}
	want := map[string]any{"cmd": "delete-device", "mac": "02:00:00:00:00:1a"}
	if len(*posts) != 2 || (*posts)[0]["mac"] != want["mac"] || (*posts)[1]["cmd"] != want["cmd"] || len((*posts)[1]) != 2 {
		t.Errorf("posts %v, want two of %v", *posts, want)
	}
	if got := c.LastForget().String(); strings.Contains(got, "00000000000000000000000000000001") || !strings.HasPrefix(got, "HTTP 200, rc ok") {
		t.Errorf("the answer as logged: %q", got)
	}

	// The parent (another model) and a MAC the controller does not list.
	*posts = nil
	if _, err := c.ForgetLaneSwitch(ctx, "02:00:00:00:00:01"); !errors.Is(err, ErrNotLaneSwitch) {
		t.Errorf("the parent's MAC: %v, want ErrNotLaneSwitch", err)
	}
	if res, err := c.ForgetLaneSwitch(ctx, "02:00:00:00:00:1b"); err != nil || res != NoRecord {
		t.Errorf("an unlisted MAC: %v, %v, want NoRecord", res, err)
	}
	if len(*posts) != 0 {
		t.Errorf("forgot %v", *posts)
	}
}

// A pending lane switch has no record: nothing to forget.
func TestForgetLaneSwitchPendingIsNoRecord(t *testing.T) {
	ok, _ := capturedForgets(t)
	c, posts := forgetServer(t, pendingList(t), ok)
	if res, err := c.ForgetLaneSwitch(context.Background(), "02:00:00:00:00:02"); err != nil || res != NoRecord || len(*posts) != 0 {
		t.Errorf("pending: %v, %v, %d posts", res, err, len(*posts))
	}
}

// A refusal is an error that quotes the status and meta, never the body.
func TestForgetLaneSwitchRefusal(t *testing.T) {
	c, _ := forgetServer(t, adoptedLaneList(t), forgetCapture{Status: http.StatusUnauthorized, Meta: map[string]string{"rc": "error", "msg": "api.err.LoginRequired"}, Records: 1})
	_, err := c.ForgetLaneSwitch(context.Background(), "02:00:00:00:00:1a")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "x_authkey") || strings.Contains(err.Error(), "00000000000000000000000000000001") {
		t.Errorf("refusal: %v", err)
	}
}
