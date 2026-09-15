package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k0ngk0ng/wire-download/internal/engine"
)

func TestRefreshRecoversLegacyAMuleLinkIdentity(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef"
	const source = "ed2k://|file|%E6%B5%8B%E8%AF%95.iso|1000|" + hash + "|/"
	for _, tc := range []struct {
		name, id, status string
		present, recover bool
	}{
		{"legacy link", source, "unknown", true, true},
		{"missing from engine", source, "unknown", false, false},
		{"removed task", source, "removed", true, false},
		{"different engine identity", strings.Repeat("a", 32), "unknown", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeBackend{}
			if tc.present {
				backend.items = []engine.Item{{ID: hash, Name: "test.iso", Status: "active", Progress: 25, DownloadRate: 64}}
			}
			s, err := NewStore(t.TempDir(), map[string]engine.Backend{"amule": backend})
			if err != nil {
				t.Fatal(err)
			}
			s.state.Jobs = []Job{{ID: "stable-job", Engine: "amule", Source: source,
				Item: engine.Item{ID: tc.id, Status: tc.status, Total: 1000, Error: "stale error"}}}
			if err := s.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			reloaded, err := NewStore(filepath.Dir(s.path), s.backends)
			if err != nil {
				t.Fatal(err)
			}
			jobs, _ := reloaded.Snapshot()
			j := jobs[0]
			if j.ID != "stable-job" || j.Source != source || backend.addCount != 0 {
				t.Fatal("recovery changed task identity or resubmitted it", j)
			}
			if tc.recover {
				if j.Item.ID != hash || j.Status != "active" || j.Error != "" || j.Completed != 250 || j.Total != 1000 || j.DownloadRate != 64 {
					t.Fatal("legacy task was not recovered and persisted", j)
				}
			} else if j.Item.ID != tc.id || j.Status != tc.status {
				t.Fatal("unrelated, missing, or removed task was changed", j)
			}
		})
	}
}
