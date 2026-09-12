package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/engine"
)

type readinessBackend struct {
	mu       sync.Mutex
	calls    int
	failures int
}

func (b *readinessBackend) Add(context.Context, string) (string, error) { return "id", nil }
func (b *readinessBackend) List(context.Context) ([]engine.Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.calls <= b.failures {
		return nil, errors.New("EC is still initializing")
	}
	return nil, nil
}
func (b *readinessBackend) Pause(context.Context, string) error  { return nil }
func (b *readinessBackend) Resume(context.Context, string) error { return nil }
func (b *readinessBackend) Remove(context.Context, string) error { return nil }
func (b *readinessBackend) Close() error                         { return nil }
func (b *readinessBackend) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func TestWaitForEnginesRetriesTransientHealth(t *testing.T) {
	dir := t.TempDir()
	aria := &readinessBackend{failures: 2}
	amule := &readinessBackend{failures: 1}
	store, err := NewStore(dir, map[string]engine.Backend{"aria2": aria, "amule": amule})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := waitForEngines(ctx, store); err != nil {
		t.Fatal(err)
	}
	if aria.callCount() < 3 || amule.callCount() < 2 {
		t.Fatalf("transient backend failures were not retried: aria2=%d amule=%d", aria.callCount(), amule.callCount())
	}
}

func TestWaitForEnginesReturnsPersistenceErrorImmediately(t *testing.T) {
	dir := t.TempDir()
	aria := &readinessBackend{}
	amule := &readinessBackend{}
	store, err := NewStore(dir, map[string]engine.Backend{"aria2": aria, "amule": amule})
	if err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(dir, "jobs-dir")
	if err := os.Mkdir(store.path, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForEngines(ctx, store); err == nil {
		t.Fatal("persistence error was swallowed")
	}
	if aria.callCount() != 1 || amule.callCount() != 1 {
		t.Fatalf("persistence error should stop after one refresh: aria2=%d amule=%d", aria.callCount(), amule.callCount())
	}
}
