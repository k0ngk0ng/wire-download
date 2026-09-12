package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/k0ngk0ng/wire-download/internal/engine"
)

type cleanupBackend struct {
	fakeBackend
	forget    func(string) error
	forgotten []string
}

func (b *cleanupBackend) Forget(_ context.Context, id string) error {
	b.forgotten = append(b.forgotten, id)
	if b.forget != nil {
		return b.forget(id)
	}
	return nil
}

func TestRefreshPersistsBeforeForgettingResult(t *testing.T) {
	for _, status := range []string{"complete", "removed", "seeding", "paused"} {
		t.Run(status, func(t *testing.T) {
			b := &cleanupBackend{}
			s, err := NewStore(t.TempDir(), map[string]engine.Backend{"aria2": b})
			if err != nil {
				t.Fatal(err)
			}
			j, err := s.Add(context.Background(), "https://example.org/fixture")
			if err != nil {
				t.Fatal(err)
			}
			b.items = []engine.Item{{ID: j.Item.ID, Status: status}}
			b.forget = func(id string) error {
				data, err := os.ReadFile(s.path)
				if err != nil {
					t.Fatal(err)
				}
				var saved State
				if err := json.Unmarshal(data, &saved); err != nil {
					t.Fatal(err)
				}
				if saved.Jobs[0].Status != status {
					t.Fatalf("forgot result before persisting %s: %+v", status, saved)
				}
				return nil
			}
			if err := s.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if status == "complete" || status == "removed" {
				want = 1
			}
			if len(b.forgotten) != want {
				t.Fatalf("%s forgotten: %v", status, b.forgotten)
			}
			b.items = nil
			if status == "complete" || status == "removed" {
				if err := s.Refresh(context.Background()); err != nil {
					t.Fatal(err)
				}
				jobs, _ := s.Snapshot()
				if jobs[0].Status != status {
					t.Fatalf("terminal state lost: %+v", jobs)
				}
			}
		})
	}
}

func TestFailedPersistenceDoesNotForgetResults(t *testing.T) {
	b := &cleanupBackend{}
	s, err := NewStore(t.TempDir(), map[string]engine.Backend{"aria2": b})
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Add(context.Background(), "https://example.org/fixture")
	if err != nil {
		t.Fatal(err)
	}
	b.items = []engine.Item{{ID: j.Item.ID, Status: "complete"}}
	// Rename cannot replace a directory, including when tests run as root.
	s.path = filepath.Join(filepath.Dir(s.path), "invalid-destination")
	if err := os.Mkdir(s.path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("expected persistence failure")
	}
	if len(b.forgotten) != 0 {
		t.Fatalf("forgot unpersisted result: %v", b.forgotten)
	}
}

func TestMetadataRecoveryPreservesLogicalIdentity(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "stored-parent"
		if legacy {
			name = "legacy-derived-parent"
		}
		t.Run(name, func(t *testing.T) {
			b := &cleanupBackend{}
			s, err := NewStore(t.TempDir(), map[string]engine.Backend{"aria2": b})
			if err != nil {
				t.Fatal(err)
			}
			j, err := s.Add(context.Background(), "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567")
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(j.ID + j.Source))
			parentID := hex.EncodeToString(sum[:8])
			s.state.Jobs[0].Item = engine.Item{ID: "old-child", Status: "paused"}
			if !legacy {
				s.state.Jobs[0].MetadataID = parentID
			}
			b.items = []engine.Item{{ID: parentID, Status: "metadata", FollowedBy: []string{"new-child"}}, {ID: "new-child", Status: "paused", Completed: 123}}
			b.forget = func(id string) error {
				reloaded, err := NewStore(filepath.Dir(s.path), s.backends)
				if err != nil {
					t.Fatal(err)
				}
				jobs, _ := reloaded.Snapshot()
				if jobs[0].ID != j.ID || jobs[0].Item.ID != "new-child" || jobs[0].MetadataID != parentID {
					t.Fatalf("handoff not durable: %+v", jobs)
				}
				return nil
			}
			if err := s.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			jobs, _ := s.Snapshot()
			if jobs[0].ID != j.ID || jobs[0].Item.ID != "new-child" || jobs[0].Completed != 123 {
				t.Fatalf("identity/progress lost: %+v", jobs)
			}
			if len(b.forgotten) != 1 || b.forgotten[0] != parentID {
				t.Fatalf("wrong cleanup: %v", b.forgotten)
			}
		})
	}
}

func TestPausedMetadataRequestCanResumeBeforeChildExists(t *testing.T) {
	b := &cleanupBackend{}
	s, err := NewStore(t.TempDir(), map[string]engine.Backend{"aria2": b})
	if err != nil {
		t.Fatal(err)
	}
	s.state.Jobs = []Job{{ID: "logical", Engine: "aria2", MetadataID: "parent", Item: engine.Item{ID: "old-child", Status: "paused"}}}
	b.items = []engine.Item{{ID: "parent", Status: "paused"}}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Snapshot()
	if jobs[0].ID != "logical" || jobs[0].Item.ID != "parent" || jobs[0].Status != "paused" {
		t.Fatalf("paused metadata lost: %+v", jobs)
	}
	if len(b.forgotten) != 0 {
		t.Fatal("paused parent forgotten")
	}
	if err := s.Action(context.Background(), "logical", "resume"); err != nil {
		t.Fatal(err)
	}
	if b.lastAction != "resume" {
		t.Fatal("cannot resume metadata")
	}
	b.items = []engine.Item{{ID: "parent", Status: "metadata", FollowedBy: []string{"new-child"}}, {ID: "new-child", Status: "active"}}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.Snapshot()
	if jobs[0].ID != "logical" || jobs[0].Item.ID != "new-child" || jobs[0].Status != "active" {
		t.Fatalf("child handoff failed: %+v", jobs)
	}
}

func TestCleanupFailureReportsHealthAndRetries(t *testing.T) {
	b := &cleanupBackend{}
	s, err := NewStore(t.TempDir(), map[string]engine.Backend{"aria2": b})
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Add(context.Background(), "https://example.org/fixture")
	if err != nil {
		t.Fatal(err)
	}
	b.items = []engine.Item{{ID: j.Item.ID, Status: "complete"}}
	b.forget = func(string) error { return os.ErrDeadlineExceeded }
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, health := s.Snapshot()
	if jobs[0].Status != "complete" || health["aria2"] == "ok" {
		t.Fatal(jobs, health)
	}
	b.forget = nil
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, health = s.Snapshot()
	if len(b.forgotten) != 2 || health["aria2"] != "ok" {
		t.Fatal(b.forgotten, health)
	}
}
