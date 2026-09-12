package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestAria2PerTorrentForceSaveOptions(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var options []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var opts map[string]string
		if len(req.Params) > 2 {
			if err := json.Unmarshal(req.Params[len(req.Params)-1], &opts); err != nil {
				t.Error(err)
				return
			}
		}
		mu.Lock()
		methods = append(methods, req.Method)
		options = append(options, opts)
		mu.Unlock()
		id := "torrent-gid"
		if req.Method == "aria2.addUri" {
			id = "magnet-gid"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":"` + id + `"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	torrentPath := filepath.Join(dir, "fixture.torrent")
	if err := os.WriteFile(torrentPath, []byte("test torrent"), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAria2(1, "secret")
	a.URL = srv.URL
	if _, err := a.AddWithID(context.Background(), torrentPath, "torrent-gid"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Add(context.Background(), "magnet:?xt=urn:btih:abc"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 || methods[0] != "aria2.addTorrent" || methods[1] != "aria2.addUri" {
		t.Fatalf("methods=%v", methods)
	}
	for i, opts := range options {
		if opts["force-save"] != "true" {
			t.Fatalf("request %d options=%v", i, opts)
		}
	}
}

func TestAria2ListConfiguresBitTorrentOnce(t *testing.T) {
	var mu sync.Mutex
	changeCount := 0
	var changedID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any = []any{}
		switch req.Method {
		case "aria2.tellActive":
			result = []map[string]any{{
				"gid": "child-gid", "status": "active", "totalLength": "100", "completedLength": "50",
				"bittorrent": map[string]any{"info": map[string]any{"name": "fixture.bin"}},
			}}
		case "aria2.changeOption":
			var id string
			if err := json.Unmarshal(req.Params[1], &id); err != nil {
				t.Error(err)
			}
			mu.Lock()
			changeCount++
			changedID = id
			mu.Unlock()
			result = nil
		case "aria2.tellWaiting", "aria2.tellStopped":
			result = []any{}
		}
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "wirectl", "result": result})
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	a := NewAria2(1, "secret")
	a.URL = srv.URL
	for i := 0; i < 2; i++ {
		items, err := a.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].ID != "child-gid" {
			t.Fatalf("items=%v", items)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if changeCount != 1 || changedID != "child-gid" {
		t.Fatalf("changeOption count=%d id=%q", changeCount, changedID)
	}
}

func TestAria2ListHandlesCompletionDuringForceSaveChange(t *testing.T) {
	var terminal bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "aria2.tellActive":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":[{"gid":"race-gid","status":"active","bittorrent":{"info":{"name":"fixture"}}}]}`))
		case "aria2.changeOption":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"Cannot change option for GID#race-gid"}}`))
		case "aria2.tellStatus":
			if terminal {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":{"gid":"race-gid","status":"complete"}}`))
			} else {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":{"gid":"race-gid","status":"active"}}`))
			}
		case "aria2.tellWaiting", "aria2.tellStopped":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":[]}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":null}`))
		}
	}))
	defer srv.Close()
	a := NewAria2(1, "secret")
	a.URL = srv.URL
	terminal = true
	if _, err := a.List(context.Background()); err != nil {
		t.Fatalf("terminal race should be ignored: %v", err)
	}
	terminal = false
	if _, err := a.List(context.Background()); err == nil {
		t.Fatal("active changeOption error was swallowed")
	}
}

func TestAria2PauseWaitsForNativePausedState(t *testing.T) {
	for _, terminal := range []string{"", "complete", "error"} {
		name := "eventually-paused"
		if terminal != "" {
			name = terminal
		}
		t.Run(name, func(t *testing.T) {
			polls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case "aria2.pause":
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":null}`))
				case "aria2.tellStatus":
					polls++
					status := "active"
					if terminal != "" {
						status = terminal
					} else if polls > 1 {
						status = "paused"
					}
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":{"gid":"pause-gid","status":"` + status + `"}}`))
				default:
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":null}`))
				}
			}))
			defer srv.Close()
			a := NewAria2(1, "secret")
			a.URL = srv.URL
			err := a.Pause(context.Background(), "pause-gid")
			if terminal == "" {
				if err != nil {
					t.Fatal(err)
				}
				if polls < 2 {
					t.Fatalf("Pause returned before native paused state: polls=%d", polls)
				}
			} else if err == nil {
				t.Fatalf("Pause accepted terminal %s task", terminal)
			}
		})
	}
}

func TestAria2ForgetIsIdempotentOnlyForMissingGID(t *testing.T) {
	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var id string
		if len(req.Params) > 1 {
			_ = json.Unmarshal(req.Params[1], &id)
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode {
		case "missing":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"GID#missing is not found"}}`))
		case "vanished":
			if req.Method == "aria2.removeDownloadResult" {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"Could not remove download result of GID#vanished"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"GID#` + id + ` is not found"}}`))
		case "active":
			if req.Method == "aria2.removeDownloadResult" {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"Could not remove download result of GID#active"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":{"gid":"active","status":"active"}}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"method not found"}}`))
		}
	}))
	defer srv.Close()
	a := NewAria2(1, "secret")
	a.URL = srv.URL
	if err := a.Forget(context.Background(), "missing"); err == nil {
		t.Fatal("non-GID RPC error was swallowed")
	}
	mode = "missing"
	if err := a.Forget(context.Background(), "missing"); err != nil {
		t.Fatal(err)
	}
	mode = "vanished"
	if err := a.Forget(context.Background(), "vanished"); err != nil {
		t.Fatal(err)
	}
	mode = "active"
	if err := a.Forget(context.Background(), "active"); err == nil {
		t.Fatal("active GID result failure was swallowed")
	}
}

func TestAria2RemoveCleansTerminalAndMissingResults(t *testing.T) {
	state := map[string]string{"active": "active", "complete": "complete"}
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var id string
		if len(req.Params) > 1 {
			_ = json.Unmarshal(req.Params[1], &id)
		}
		mu.Lock()
		calls = append(calls, req.Method+":"+id)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		status, found := state[id]
		mu.Unlock()
		if req.Method == "aria2.tellStatus" {
			if !found {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"GID#` + id + ` is not found"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":{"gid":"` + id + `","status":"` + status + `"}}`))
			return
		}
		if req.Method == "aria2.remove" {
			mu.Lock()
			state[id] = "removed"
			mu.Unlock()
		}
		if req.Method == "aria2.removeDownloadResult" {
			if !found {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"GID#` + id + ` is not found"}}`))
				return
			}
			mu.Lock()
			delete(state, id)
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":null}`))
	}))
	defer srv.Close()
	a := NewAria2(1, "secret")
	a.URL = srv.URL
	if err := a.Remove(context.Background(), "active"); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove(context.Background(), "complete"); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove(context.Background(), "missing"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"aria2.tellStatus:active", "aria2.remove:active", "aria2.tellStatus:active", "aria2.removeDownloadResult:active",
		"aria2.tellStatus:complete", "aria2.removeDownloadResult:complete",
		"aria2.tellStatus:missing", "aria2.removeDownloadResult:missing",
	}
	if !bytes.Equal([]byte(strings.Join(calls, "\n")), []byte(strings.Join(want, "\n"))) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestAria2RemoveMetadataParentCleansFollowedBy(t *testing.T) {
	for _, race := range []bool{false, true} {
		name := "already-complete"
		if race {
			name = "completes-during-remove"
		}
		t.Run(name, func(t *testing.T) {
			type result struct {
				status   string
				followed []string
			}
			state := map[string]result{
				"parent": {status: "active"},
				"child":  {status: "active"},
			}
			if !race {
				state["parent"] = result{status: "complete", followed: []string{"child"}}
			}
			var mu sync.Mutex
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string            `json:"method"`
					Params []json.RawMessage `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				var id string
				if len(req.Params) > 1 {
					_ = json.Unmarshal(req.Params[1], &id)
				}
				mu.Lock()
				calls = append(calls, req.Method+":"+id)
				current, found := state[id]
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if req.Method == "aria2.tellStatus" {
					if !found {
						_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"GID#` + id + ` is not found"}}`))
						return
					}
					body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "wirectl", "result": map[string]any{
						"gid": id, "status": current.status, "followedBy": current.followed,
					}})
					_, _ = w.Write(body)
					return
				}
				if req.Method == "aria2.remove" {
					if race && id == "parent" {
						mu.Lock()
						state["parent"] = result{status: "complete", followed: []string{"child"}}
						mu.Unlock()
						_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"Cannot remove GID#parent"}}`))
						return
					}
					mu.Lock()
					state[id] = result{status: "removed"}
					mu.Unlock()
				}
				if req.Method == "aria2.removeDownloadResult" {
					if !found {
						_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","error":{"code":1,"message":"GID#` + id + ` is not found"}}`))
						return
					}
					mu.Lock()
					delete(state, id)
					mu.Unlock()
				}
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"wirectl","result":null}`))
			}))
			defer srv.Close()
			a := NewAria2(1, "secret")
			a.URL = srv.URL
			if err := a.Remove(context.Background(), "parent"); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()
			if _, ok := state["child"]; ok {
				t.Fatalf("metadata child survived parent removal; calls=%v", calls)
			}
			parentForget, childForget := -1, -1
			for i, call := range calls {
				if call == "aria2.removeDownloadResult:parent" {
					parentForget = i
				}
				if call == "aria2.removeDownloadResult:child" {
					childForget = i
				}
			}
			if childForget < 0 || parentForget < 0 || childForget > parentForget {
				t.Fatalf("child was not forgotten before parent: calls=%v", calls)
			}
		})
	}
}
