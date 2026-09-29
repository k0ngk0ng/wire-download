package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/client"
	"github.com/k0ngk0ng/wire-download/internal/config"
)

func daemonTestServer(t *testing.T, handler http.Handler) (string, *client.Client) {
	t.Helper()
	base := filepath.Join("..", "..", ".cache", "tmp")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "dc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if _, err := config.Init(dir, filepath.Join(dir, "downloads")); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return dir, client.New(dir)
}

func TestRestartDoesNotStartAfterStatusOrShutdownFailure(t *testing.T) {
	for _, failPath := range []string{"/v1/status", "/v1/shutdown"} {
		t.Run(failPath, func(t *testing.T) {
			var calls atomic.Int32
			dir, c := daemonTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == failPath {
					w.WriteHeader(500)
					_, _ = w.Write([]byte(`{"error":"fixture failure"}`))
					return
				}
				_, _ = w.Write([]byte(`{"jobs":[],"engines":{}}`))
			}))
			err := daemonCommand(context.Background(), dir, c, []string{"restart"})
			if err == nil || !strings.Contains(err.Error(), "fixture failure") {
				t.Fatalf("error lost: %v", err)
			}
			want := int32(1)
			if failPath == "/v1/shutdown" {
				want = 2
			}
			if calls.Load() != want {
				t.Fatalf("unexpected retry/start: %d calls", calls.Load())
			}
			if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
				t.Fatal("attempted startup after failure")
			}
		})
	}
}

func TestRestartValidatesConfigBeforeStopping(t *testing.T) {
	dir, c := daemonTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid config must not contact/stop the daemon")
	}))
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := daemonCommand(context.Background(), dir, c, []string{"restart"}); err == nil {
		t.Fatal("accepted broken config")
	}
}

func TestRestartCancellationWaitsForCleanupWithoutStarting(t *testing.T) {
	dir, c := daemonTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[],"engines":{}}`))
	}))
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = daemonCommand(ctx, dir, c, []string{"restart"})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("restart did not wait for lock release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
		t.Fatal("attempted startup while daemon was cleaning up")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if stopped, err := daemonStopped(dir); !stopped || err != nil {
		t.Fatalf("not stopped after lock release: %v, %v", stopped, err)
	}
}
