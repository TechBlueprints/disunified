package apcbackups

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCard is the card's session behaviour as observed live: a login sets
// a session cookie and redirects to /NMC/<token>/; a page needs a cookie
// whose session is alive; a login that carries a dead session's cookie is
// answered 400; logout ends the session. Drop() kills every session, as
// the card did once on 2026-09-27.
type fakeCard struct {
	mu       sync.Mutex
	sessions map[string]bool // cookie value -> alive
	logins   int
	n        int
}

func (f *fakeCard) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/Forms/login1":
		f.logins++
		if c, err := r.Cookie("APCSESS"); err == nil && !f.sessions[c.Value] {
			http.Error(w, "Bad Request", http.StatusBadRequest) // the poisoned-jar answer
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("login_username") != "apc" || r.Form.Get("login_password") != "secret" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		f.n++
		tok := fmt.Sprintf("tok%d", f.n)
		f.sessions[tok] = true
		http.SetCookie(w, &http.Cookie{Name: "APCSESS", Value: tok, Path: "/"})
		w.Header().Set("Location", "/NMC/"+tok+"/home.htm")
		w.WriteHeader(http.StatusSeeOther)
	case strings.HasPrefix(r.URL.Path, "/NMC/"):
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/NMC/"), "/", 2)
		tok := parts[0]
		c, err := r.Cookie("APCSESS")
		if err != nil || c.Value != tok || !f.sessions[tok] {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if len(parts) == 2 && parts[1] == "logout.htm" {
			delete(f.sessions, tok)
		}
		fmt.Fprintf(w, "<html><body>page %s of %s</body></html>", parts[1], tok)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCard) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.sessions {
		f.sessions[k] = false
	}
}

// Every poll is its own session: after the card drops a session behind
// the driver's back, the next poll logs in afresh instead of carrying the
// dead cookie forever.
func TestEachPollIsAFreshSession(t *testing.T) {
	card := &fakeCard{sessions: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(card.handler))
	defer srv.Close()
	w := NewWeb(srv.URL, "apc", "secret")
	pages, err := w.Pages(context.Background(), "home", "ulabout")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pages["home"], "page home.htm") || !strings.Contains(pages["ulabout"], "page ulabout.htm") {
		t.Errorf("pages = %v", pages)
	}
	card.drop() // the card invalidates everything, including the session the jar remembers
	if _, err := w.Pages(context.Background(), "home"); err != nil {
		t.Fatalf("second poll after the card dropped the session: %v", err)
	}
	if card.logins != 2 {
		t.Errorf("logins = %d, want one per poll", card.logins)
	}
	// Bad credentials are reported as such.
	bad := NewWeb(srv.URL, "apc", "wrong")
	if _, err := bad.Pages(context.Background(), "home"); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("bad credentials: %v", err)
	}
}
