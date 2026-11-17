package unifiapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A device the controller lists without a record id is pending adoption:
// UpdateDevice must not send the PUT (it would go to rest/device/ and be
// answered api.err.IdRequired, seen live on 2026-11-16) and says why.
func TestUpdateDeviceWritesNothingWithoutARecordID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request may be made for a device without a record id; got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c := New(srv.URL, "0123456789abcdef0123456789abcdef", "default", true)
	err := c.UpdateDevice(context.Background(), "", map[string]any{"name": "x"})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("err = %v, want ErrPending", err)
	}
}
