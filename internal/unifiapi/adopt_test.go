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

// The controller's list with an automatic lane switch pending beside its
// adopted parent (Network 11.0.99, 2026-11-17, before the first automatic
// adoption; the parent's record reduced): 02:00:00:00:00:02 has no _id,
// "adopted": false, "state": 2.
func pendingList(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "stat-device-lane-switch-pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAdoptPendingAsksForExactlyOneMAC(t *testing.T) {
	body := pendingList(t)
	var posts []map[string]any
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/s/default/stat/device"):
			w.Write(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/s/default/cmd/devmgr"):
			if fail {
				http.Error(w, `{"meta":{"rc":"error","msg":"api.err.LoginRequired"}}`, http.StatusUnauthorized)
				return
			}
			b, _ := io.ReadAll(r.Body)
			var p map[string]any
			if err := json.Unmarshal(b, &p); err != nil {
				t.Error(err)
			}
			posts = append(posts, p)
			w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "0123456789abcdef0123456789abcdef", "default", true)
	ctx := context.Background()

	// The pending lane switch: one command, that MAC and nothing else.
	if err := c.AdoptPending(ctx, "02:00:00:00:00:02"); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || len(posts[0]) != 2 || posts[0]["cmd"] != "adopt" || posts[0]["mac"] != "02:00:00:00:00:02" {
		t.Fatalf("posts %v, want one {cmd adopt, mac 02:00:00:00:00:02}", posts)
	}
	// Its adopted parent: listed with a record, so nothing is asked.
	if err := c.AdoptPending(ctx, "02:00:00:00:00:01"); !errors.Is(err, ErrNotPending) {
		t.Errorf("an adopted device: err %v, want ErrNotPending", err)
	}
	// A device the controller does not list: nothing is asked.
	if err := c.AdoptPending(ctx, "02:00:00:00:00:99"); !errors.Is(err, ErrNotListed) {
		t.Errorf("an unlisted device: err %v, want ErrNotListed", err)
	}
	if len(posts) != 1 {
		t.Errorf("%d adopt commands sent, want 1", len(posts))
	}
	// A refused command is an error.
	fail = true
	if err := c.AdoptPending(ctx, "02:00:00:00:00:02"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused command: err %v", err)
	}
}
