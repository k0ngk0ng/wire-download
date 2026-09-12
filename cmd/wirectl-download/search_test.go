package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/client"
	"github.com/k0ngk0ng/wire-download/internal/search"
)

func TestSearchDurationAndSourceParsing(t *testing.T) {
	for _, test := range []struct {
		input time.Duration
		want  int
	}{
		{5 * time.Second, 5},
		{5100 * time.Millisecond, 6},
		{3 * time.Minute, 180},
	} {
		got, err := durationSeconds(test.input)
		if err != nil || got != test.want {
			t.Fatalf("durationSeconds(%s)=%d,%v; want %d,nil", test.input, got, err, test.want)
		}
	}
	if _, err := durationSeconds(0); err == nil {
		t.Fatal("zero timeout accepted")
	}
	if got := splitSearchSources(" nyaa, dmhy,nyaa,, dmhy "); len(got) != 2 || got[0] != "nyaa" || got[1] != "dmhy" {
		t.Fatalf("sources=%#v", got)
	}
}

func TestSearchProgressLineContainsSourceStateWithoutControls(t *testing.T) {
	line := searchProgressLine(search.Snapshot{
		ID: "abc", Status: "running", Total: 2,
		Sources: []search.SourceState{{Name: "Feed\n", Status: "failed", Progress: 42, Count: 2, Error: "bad\x1b[31m"}},
	})
	if !strings.Contains(line, "abc") || !strings.Contains(line, "42%") || !strings.Contains(line, "bad") {
		t.Fatalf("progress line=%q", line)
	}
	if strings.ContainsAny(line, "\x1b\n\r") {
		t.Fatalf("progress line contains controls: %q", line)
	}
}

func unixSearchClient(t *testing.T, handler http.Handler) (*client.Client, func()) {
	t.Helper()
	// macOS limits Unix socket paths to roughly 104 bytes. Keep the test
	// directory under the repository instead of nesting it under t.TempDir's
	// long system path.
	base := filepath.Join("..", "..", ".cache", "tmp")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "sc-")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "daemon.sock"))
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	return client.New(dir), func() {
		_ = server.Close()
		_ = os.RemoveAll(dir)
	}
}

func TestPollSearchPrintsFailedSnapshotBeforeReturningError(t *testing.T) {
	var calls atomic.Int32
	c, cleanup := unixSearchClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/searches/s-failed" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(search.Snapshot{
			ID: "s-failed", Status: "failed", Total: 0,
			Sources: []search.SourceState{{Name: "offline", Status: "failed", Error: "upstream unavailable"}},
		})
	}))
	defer cleanup()
	var out, progress bytes.Buffer
	err := pollSearchCommand(context.Background(), c, search.Snapshot{ID: "s-failed", Status: "running"}, true, &out, &progress)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("poll error=%v", err)
	}
	if calls.Load() == 0 || !strings.Contains(out.String(), `"status":"failed"`) || !strings.Contains(progress.String(), "offline") {
		t.Fatalf("output=%q progress=%q calls=%d", out.String(), progress.String(), calls.Load())
	}
}

func TestPollSearchCancelsDaemonWhenContextEnds(t *testing.T) {
	var cancels atomic.Int32
	c, cleanup := unixSearchClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/searches/s-cancel":
			_ = json.NewEncoder(w).Encode(search.Snapshot{ID: "s-cancel", Status: "running"})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/searches/s-cancel":
			cancels.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	defer cancel()
	var out, progress bytes.Buffer
	err := pollSearchCommand(ctx, c, search.Snapshot{ID: "s-cancel", Status: "running"}, false, &out, &progress)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("poll error=%v", err)
	}
	if cancels.Load() != 1 {
		t.Fatalf("daemon cancel requests=%d", cancels.Load())
	}
}
