package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k0ngk0ng/wire-download/internal/engine"
)

type fakeBackend struct {
	items      []engine.Item
	addCount   int
	err        error
	lastAction string
}

func (f *fakeBackend) Add(context.Context, string) (string, error) { f.addCount++; return "abc", f.err }
func (f *fakeBackend) List(context.Context) ([]engine.Item, error) { return f.items, f.err }
func (f *fakeBackend) Pause(context.Context, string) error         { f.lastAction = "pause"; return f.err }
func (f *fakeBackend) Resume(context.Context, string) error        { f.lastAction = "resume"; return f.err }
func (f *fakeBackend) Remove(context.Context, string) error        { f.lastAction = "remove"; return f.err }
func (f *fakeBackend) Close() error                                { return nil }
func newTestStore(t *testing.T) (*Store, *fakeBackend) {
	t.Helper()
	f := &fakeBackend{}
	s, err := NewStore(t.TempDir(), map[string]engine.Backend{"aria2": f, "amule": f})
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}
func TestPersistentIdentityAndDedupe(t *testing.T) {
	s, f := newTestStore(t)
	ctx := context.Background()
	j, err := s.Add(ctx, "https://example.org/legal.iso")
	if err != nil {
		t.Fatal(err)
	}
	if j.ID == j.Item.ID || j.Item.ID != "abc" {
		t.Fatal("job and engine identities must be distinct", j)
	}
	again, err := s.Add(ctx, j.Source)
	if err != nil || again.ID != j.ID || f.addCount != 1 {
		t.Fatal(again, err, f.addCount)
	}
	reloaded, err := NewStore(filepath.Dir(s.path), s.backends)
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := reloaded.Snapshot()
	if jobs[0].Item.ID != "abc" || jobs[0].ID != j.ID {
		t.Fatal("lost identity on disk", jobs)
	}
	f.items = []engine.Item{{ID: "abc", Status: "active", Total: 100, Completed: 30, Progress: 30}}
	if err = s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Action(ctx, j.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if f.lastAction != "pause" {
		t.Fatal(f)
	}
	f.items = nil
	if err = s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.Snapshot()
	if jobs[0].Status != "unknown" {
		t.Fatal("missing engine task must not be marked complete")
	}
}
func TestRefreshKeepsSnapshotWhenEngineFails(t *testing.T) {
	s, f := newTestStore(t)
	j, err := s.Add(context.Background(), "https://example.org/x")
	if err != nil {
		t.Fatal(err)
	}
	f.err = errors.New("offline")
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, health := s.Snapshot()
	if jobs[0].Status != j.Status || health["aria2"] != "offline" {
		t.Fatal(jobs, health)
	}
}
func TestCorruptDatabaseRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jobs.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, nil); err == nil {
		t.Fatal("corrupt database accepted")
	}
}
func TestValidateSources(t *testing.T) {
	for _, source := range []string{"ed2k://|file|test.bin|100|0123456789abcdef0123456789abcdef|/", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "https://example.org/x.torrent"} {
		if _, err := ValidateSource(source); err != nil {
			t.Errorf("%s: %v", source, err)
		}
	}
	for _, source := range []string{"ed2k://|file|x|0|0123456789abcdef0123456789abcdef|/", "ed2k://|server|127.0.0.1|1234|/", "magnet:?xt=urn:btmh:bad", "magnet:?xt=urn:btih:nope", "file:///etc/passwd", "https://example.org/\nfoo"} {
		if _, err := ValidateSource(source); err == nil {
			t.Errorf("accepted %q", source)
		}
	}
}
func TestControlAPI(t *testing.T) {
	s, _ := newTestStore(t)
	stopped := false
	h := Handler(s, func() { stopped = true })
	request := func(method, path, body, host, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = host
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/v1/status", "", "evil.example", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := request("POST", "/v1/shutdown", "", "localhost", "https://evil.example"); w.Code != 403 || stopped {
		t.Fatal(w.Code)
	}
	w := request("POST", "/v1/jobs", `{"source":"https://example.org/x"}`, "localhost", "")
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	var j Job
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	if w = request("POST", "/v1/jobs/"+j.ID+"/pause", "", "localhost", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("POST", "/v1/jobs/nope/pause", "", "localhost", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w = request("POST", "/v1/shutdown", "", "localhost", ""); w.Code != 200 || !stopped {
		t.Fatal(w.Code)
	}
}
func TestDaemonLock(t *testing.T) {
	dir := t.TempDir()
	f, err := acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if second, err := acquire(dir); err == nil {
		second.Close()
		t.Fatal("second daemon acquired lock")
	}
}
func TestMergeINIPreservesIdentity(t *testing.T) {
	out := mergeINI("[eMule]\nUserHash=abc\nPort=1\n[ExternalConnect]\nECPort=2\n", map[string]map[string]string{"eMule": {"Port": "5"}, "ExternalConnect": {"ECPort": "6"}})
	if !strings.Contains(out, "UserHash=abc") || strings.Contains(out, "Port=1") || strings.Count(out, "Port=5") != 1 {
		t.Fatal(out)
	}
}
