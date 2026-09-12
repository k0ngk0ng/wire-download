package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAria2AuthAndStableGID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if string(req.Params[0]) != `"token:secret"` {
			t.Error("missing auth")
		}
		if req.Method != "aria2.addUri" {
			t.Error(req.Method)
		}
		var opts map[string]string
		if err := json.Unmarshal(req.Params[2], &opts); err != nil {
			t.Error(err)
		}
		if opts["gid"] != "1234567890abcdef" {
			t.Error(opts)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":"1234567890abcdef"}`))
	}))
	defer srv.Close()
	a := NewAria2(1, "secret")
	a.URL = srv.URL
	id, err := a.AddWithID(context.Background(), "magnet:?xt=urn:btih:abc", "1234567890abcdef")
	if err != nil || id != "1234567890abcdef" {
		t.Fatal(id, err)
	}
}
func TestAria2ErrorRedactsSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":{"code":1,"message":"bad secret"}}`))
	}))
	defer srv.Close()
	a := NewAria2(1, "secret")
	a.URL = srv.URL
	if err := a.Pause(context.Background(), "x"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}
func TestAriaStatus(t *testing.T) {
	s := ariaStatus{GID: "1", Status: "active", Total: "100", Completed: "100", Down: "0", Up: "123"}
	i := s.item()
	if i.Status != "seeding" || i.Progress != 100 || i.UploadRate != 123 {
		t.Fatal(i)
	}
}
