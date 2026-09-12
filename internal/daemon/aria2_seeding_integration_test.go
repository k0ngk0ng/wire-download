package daemon

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/engine"
)

func testBencode(v any) []byte {
	switch x := v.(type) {
	case string:
		return append([]byte(strconv.Itoa(len(x))+":"), []byte(x)...)
	case []byte:
		return append([]byte(strconv.Itoa(len(x))+":"), x...)
	case int:
		return []byte(fmt.Sprintf("i%de", x))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := []byte{'d'}
		for _, k := range keys {
			out = append(out, testBencode(k)...)
			out = append(out, testBencode(x[k])...)
		}
		return append(out, 'e')
	default:
		panic(fmt.Sprintf("unsupported bencode value %T", v))
	}
}

func testFreeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func startTestAria2Seeder(t *testing.T, binary, dir, session string, rpcPort, btPort int) (*exec.Cmd, *engine.Aria2) {
	t.Helper()
	logFile, err := os.OpenFile(filepath.Join(dir, "aria2.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--no-conf=true", "--enable-rpc=true", "--rpc-listen-all=false",
		fmt.Sprintf("--rpc-listen-port=%d", rpcPort), "--rpc-secret=seeding-test-secret",
		"--dir=" + dir, "--save-session=" + session, "--input-file=" + session,
		"--force-save=false", "--max-download-result=10000", "--keep-unfinished-download-result=true",
		"--file-allocation=none", "--allow-overwrite=false", "--check-integrity=true",
		"--enable-dht=false", "--enable-dht6=false", "--enable-peer-exchange=false",
		"--bt-enable-lpd=false", "--bt-external-ip=127.0.0.1", "--seed-ratio=0", "--seed-time=600",
		fmt.Sprintf("--listen-port=%d", btPort), "--summary-interval=0", "--console-log-level=notice",
	}
	cmd := exec.Command(binary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatal(err)
	}
	logFile.Close()
	aria := engine.NewAria2(rpcPort, "seeding-test-secret")
	ready := context.Background()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		ctx, cancel := context.WithTimeout(ready, time.Second)
		err = aria.Call(ctx, "getVersion", nil, nil)
		cancel()
		if err == nil {
			return cmd, aria
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	logFile.Close()
	t.Fatalf("aria2 did not become ready; inspect %s", logFile.Name())
	return nil, nil
}

func stopTestAria2(t *testing.T, cmd *exec.Cmd, aria *engine.Aria2) {
	t.Helper()
	if cmd == nil || cmd.ProcessState != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = aria.Call(ctx, "shutdown", nil, nil)
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func TestRealAria2SeedingSessionRecoveryAndRemoval(t *testing.T) {
	binary := os.Getenv("WIRECTL_TEST_ARIA2")
	if binary == "" {
		t.Skip("set WIRECTL_TEST_ARIA2 to run real seeding session test")
	}
	root := t.TempDir()
	downloads := filepath.Join(root, "downloads")
	if err := os.Mkdir(downloads, 0700); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "session.txt")
	if err := os.WriteFile(session, nil, 0600); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("wire-download seeding integration fixture\n"), 1024)
	name := "seed-fixture.bin"
	if err := os.WriteFile(filepath.Join(downloads, name), payload, 0600); err != nil {
		t.Fatal(err)
	}
	rpcPort := testFreeTCPPort(t)
	btPort := testFreeTCPPort(t)
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peers := append([]byte{127, 0, 0, 1}, byte(btPort>>8), byte(btPort))
		body := testBencode(map[string]any{"interval": 1, "complete": 1, "incomplete": 0, "peers": peers})
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer tracker.Close()
	var pieces []byte
	for offset := 0; offset < len(payload); offset += 16384 {
		sum := sha1.Sum(payload[offset:min(offset+16384, len(payload))])
		pieces = append(pieces, sum[:]...)
	}
	info := map[string]any{"name": name, "length": len(payload), "piece length": 16384, "pieces": pieces}
	torrent := testBencode(map[string]any{"announce": tracker.URL, "info": info})
	torrentPath := filepath.Join(root, "seed.torrent")
	if err := os.WriteFile(torrentPath, torrent, 0600); err != nil {
		t.Fatal(err)
	}

	cmd, aria := startTestAria2Seeder(t, binary, downloads, session, rpcPort, btPort)
	defer stopTestAria2(t, cmd, aria)
	gid, err := aria.Add(context.Background(), torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	var seeded engine.Item
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		items, listErr := aria.List(context.Background())
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, item := range items {
			if item.ID == gid {
				seeded = item
			}
		}
		if seeded.Status == "seeding" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if seeded.Status != "seeding" || seeded.Progress != 100 {
		t.Fatalf("torrent did not enter seeding: %#v", seeded)
	}

	store, err := NewStore(t.TempDir(), map[string]engine.Backend{"aria2": aria})
	if err != nil {
		t.Fatal(err)
	}
	store.state.Jobs = []Job{{ID: "job-seeding", Engine: "aria2", Source: torrentPath, Item: seeded}}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	if err := aria.Call(context.Background(), "saveSession", nil, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(session)
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(saved)) == 0 {
		t.Fatal("force-save=true did not serialize seeding task")
	}
	stopTestAria2(t, cmd, aria)
	cmd = nil

	restartedCmd, restarted := startTestAria2Seeder(t, binary, downloads, session, rpcPort, btPort)
	defer stopTestAria2(t, restartedCmd, restarted)
	store.backends["aria2"] = restarted
	var recovered Job
	var lastRefreshErr error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if err := store.Refresh(context.Background()); err != nil {
			lastRefreshErr = err
		} else {
			jobs, _ := store.Snapshot()
			if len(jobs) == 1 && jobs[0].Status == "seeding" && jobs[0].Progress == 100 {
				recovered = jobs[0]
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if recovered.Status != "seeding" || recovered.Progress != 100 {
		jobs, _ := store.Snapshot()
		t.Fatalf("persisted seeding task did not recover after restart: jobs=%#v last_refresh_error=%v", jobs, lastRefreshErr)
	}
	recoveredStatus, recoveredProgress := recovered.Status, recovered.Progress
	jobs, _ := store.Snapshot()
	if err := store.Action(context.Background(), "job-seeding", "remove"); err != nil {
		t.Fatal(err)
	}
	jobs, _ = store.Snapshot()
	if len(jobs) != 1 || jobs[0].Status != "removed" {
		t.Fatalf("removed seeding task has wrong Store state: %#v", jobs)
	}
	cleared, err := os.ReadFile(session)
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(cleared)) != 0 {
		t.Fatalf("removed seeding task remained in session:\n%s", cleared)
	}

	// Removing an already removed job and forgetting its absent GID are safe
	// retries. These calls also flush the empty session again.
	if err := store.Action(context.Background(), "job-seeding", "remove"); err != nil {
		t.Fatalf("repeated Store remove: %v", err)
	}
	if err := restarted.Forget(context.Background(), gid); err != nil {
		t.Fatalf("repeated Forget: %v", err)
	}
	if err := restarted.Forget(context.Background(), gid); err != nil {
		t.Fatalf("second repeated Forget: %v", err)
	}

	stopTestAria2(t, restartedCmd, restarted)
	restartedCmd = nil
	finalCmd, finalAria := startTestAria2Seeder(t, binary, downloads, session, rpcPort, btPort)
	defer stopTestAria2(t, finalCmd, finalAria)
	items, err := finalAria.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == gid {
			t.Fatalf("removed seeding task was resurrected after restart: %#v", item)
		}
	}
	store.backends["aria2"] = finalAria
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, _ = store.Snapshot()
	if len(jobs) != 1 || jobs[0].Status != "removed" {
		t.Fatalf("removed state changed after final restart: %#v", jobs)
	}
	t.Logf("seeding recovery evidence: status=%s progress=%.0f session_bytes=%d recovered_status=%s recovered_progress=%.0f removed_session_bytes=%d", seeded.Status, seeded.Progress, len(saved), recoveredStatus, recoveredProgress, len(cleared))
}
