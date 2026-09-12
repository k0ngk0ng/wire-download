package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/engine"
	"github.com/k0ngk0ng/wire-download/internal/search"
)

func TestSearchRoutesProgressDownloadAndSources(t *testing.T) {
	dir := t.TempDir()
	if err := search.SaveSources(dir, []search.SourceConfig{{ID: "ed2k", Name: "eMule", Type: "emule", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	released := make(chan struct{})
	service, err := NewSearchService(context.Background(), dir, func(ctx context.Context, _ search.Request, emit search.Emit) error {
		emit([]search.Result{{Name: "owned fixture", Link: "ed2k://|file|fixture.bin|128|" + strings.Repeat("a", 32) + "|/"}}, 50)
		close(ready)
		select {
		case <-released:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.manager.Close()
	store, err := NewStore(dir, map[string]engine.Backend{"amule": &readinessBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler(store, func() {}, service)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request("POST", "/v1/searches", `{"query":"fixture","type":"ed2k"}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var snapshot search.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("search not started")
	}
	// An unfinished search must not block status, result polling, or enqueueing.
	w = request("GET", "/v1/status", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request("GET", "/v1/searches/"+snapshot.ID, "")
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Done() || len(snapshot.Results) != 1 || snapshot.Sources[0].Progress != 50 {
		t.Fatalf("%+v", snapshot)
	}
	w = request("POST", "/v1/searches/"+snapshot.ID+"/results/"+snapshot.Results[0].ID+"/download", "")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var downloaded Job
	if err := json.Unmarshal(w.Body.Bytes(), &downloaded); err != nil {
		t.Fatal(err)
	}
	if downloaded.Engine != "amule" || downloaded.Source != snapshot.Results[0].Link {
		t.Fatalf("%+v", downloaded)
	}
	close(released)
	w = request("POST", "/v1/searches/"+snapshot.ID+"/results/missing/download", "")
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = request("PUT", "/v1/search-sources/private", `{"id":"private","name":"Private","type":"torznab","url":"https://example.org/api","enabled":true,"api_key":"private-test-key"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("GET", "/v1/search-sources", "")
	if strings.Contains(w.Body.String(), "private-test-key") {
		t.Fatal("API key disclosed")
	}
	w = request("PATCH", "/v1/search-sources/private", `{"enabled":false}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	loaded, err := search.LoadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || loaded[1].Enabled || loaded[1].APIKey != "private-test-key" {
		t.Fatal("toggle lost key or not persisted")
	}
	info, err := os.Stat(filepath.Join(dir, "search-sources.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("source config permissions", err)
	}
	w = request("DELETE", "/v1/search-sources/private", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request("GET", "/v1/searches/missing", "")
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("GET", "http://localhost/v1/search-sources", nil)
	r.Header.Set("Origin", "https://example.org")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("browser origin accepted")
	}
}
