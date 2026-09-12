package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/config"
	"github.com/k0ngk0ng/wire-download/internal/engine"
)

func acquire(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another daemon is running")
	}
	return f, nil
}
func UpdateServers(ctx context.Context, dir string) error {
	lock, err := acquire(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	c, err := config.Load(dir)
	if err != nil {
		return err
	}
	return BootstrapServers(ctx, dir, c.ServerListURLs, true)
}
func Run(ctx context.Context, dir string, c config.Config) error {
	lock, err := acquire(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	defer os.Remove(filepath.Join(dir, "daemon.pid"))
	if err = Prepare(dir, c); err != nil {
		return err
	}
	for _, port := range []int{c.Aria2Port, c.AMulePort} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return fmt.Errorf("engine port %d is already in use: %w", port, err)
		}
		ln.Close()
	}
	if err = BootstrapServers(ctx, dir, c.ServerListURLs, false); err != nil {
		return err
	}
	ab := engine.NewAria2(c.Aria2Port, c.Secret)
	eb := engine.NewAMule(c.AMulecmdBinary, filepath.Join(dir, "amule"), c.Secret, c.AMulePort)
	defer ab.Close()
	defer eb.Close()
	store, err := NewStore(dir, map[string]engine.Backend{"aria2": ab, "amule": eb})
	if err != nil {
		return err
	}
	closeProxy, err := startAuthProxy(ctx, dir, c, store)
	if err != nil {
		return err
	}
	defer closeProxy()
	aria, err := startProcess(c.Aria2Binary, []string{"--conf-path=" + filepath.Join(dir, "aria2", "aria2.conf")}, filepath.Join(dir, "logs", "aria2.log"))
	if err != nil {
		return err
	}
	defer aria.stop()
	amule, err := startProcess(c.AMuledBinary, []string{"--config-dir=" + filepath.Join(dir, "amule")}, filepath.Join(dir, "logs", "amuled.log"))
	if err != nil {
		return err
	}
	defer amule.stop()
	ready, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for _, p := range []struct {
		proc *process
		port int
	}{{aria, c.Aria2Port}, {amule, c.AMulePort}} {
		if err = waitPort(ready, p.proc, p.port); err != nil {
			return err
		}
	}
	if err = store.Refresh(ready); err != nil {
		return err
	}
	_, health := store.Snapshot()
	for name, status := range health {
		if status != "ok" {
			return fmt.Errorf("%s readiness failed: %s", name, status)
		}
	}
	// aMule needs an explicit connect command after startup.
	if err = eb.Connect(ready); err != nil {
		log.Printf("eMule connect: %v", err)
	}
	socket := filepath.Join(dir, "daemon.sock")
	if err = os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(dir, "daemon.pid"), []byte(strconv.Itoa(os.Getpid()))); err != nil {
		return err
	}
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	srv := &http.Server{Handler: Handler(store, stop), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	log.Printf("wirectl ready: %s", socket)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var runErr error
loop:
	for {
		select {
		case <-serveCtx.Done():
			break loop
		case err = <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				runErr = err
			}
			break loop
		case <-aria.done:
			runErr = fmt.Errorf("aria2 exited unexpectedly: %v", aria.err)
			break loop
		case <-amule.done:
			runErr = fmt.Errorf("amuled exited unexpectedly: %v", amule.err)
			break loop
		case <-ticker.C:
			pollCtx, cancel := context.WithTimeout(serveCtx, 20*time.Second)
			err = store.Refresh(pollCtx)
			cancel()
			if err != nil {
				runErr = fmt.Errorf("persist tasks: %w", err)
				break loop
			}
		}
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdown)
	return runErr
}
func Handler(store *Store, stop context.CancelFunc) http.Handler {
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, code int, err error) {
		respond(w, code, map[string]string{"error": err.Error()})
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		jobs, health := store.Snapshot()
		respond(w, 200, map[string]any{"version": "1", "jobs": jobs, "engines": health})
	})
	mux.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Source string `json:"source"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			fail(w, 400, err)
			return
		}
		job, err := store.Add(r.Context(), req.Source)
		if err != nil {
			fail(w, 422, err)
			return
		}
		respond(w, 201, job)
	})
	mux.HandleFunc("POST /v1/jobs/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		if action != "pause" && action != "resume" && action != "remove" {
			fail(w, 400, errors.New("invalid action"))
			return
		}
		err := store.Action(r.Context(), r.PathValue("id"), action)
		if errors.Is(err, os.ErrNotExist) {
			fail(w, 404, err)
			return
		}
		if err != nil {
			fail(w, 422, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]bool{"ok": true}); stop() })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "localhost" {
			fail(w, 403, errors.New("invalid control host"))
			return
		}
		if strings.TrimSpace(r.Header.Get("Origin")) != "" {
			fail(w, 403, errors.New("browser origins are not accepted"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}
