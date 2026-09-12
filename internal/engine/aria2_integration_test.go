package engine

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func realAria(t *testing.T, dir string, btPort int) *Aria2 {
	t.Helper()
	binaryPath := os.Getenv("WIRECTL_TEST_ARIA2")
	if binaryPath == "" {
		t.Skip("set WIRECTL_TEST_ARIA2 to run real protocol tests")
	}
	rpcPort := freePort(t)
	logFile, err := os.Create(filepath.Join(dir, "engine.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binaryPath, "--no-conf=true", "--enable-rpc=true", "--rpc-listen-all=false", fmt.Sprintf("--rpc-listen-port=%d", rpcPort), "--rpc-secret=test-integration-secret", "--dir="+dir, fmt.Sprintf("--listen-port=%d", btPort), "--enable-dht=false", "--enable-dht6=false", "--enable-peer-exchange=false", "--bt-enable-lpd=false", "--bt-external-ip=127.0.0.1", "--seed-ratio=0", "--seed-time=10", "--file-allocation=none", "--check-integrity=true", "--summary-interval=0", "--console-log-level=notice")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err = cmd.Start(); err != nil {
		logFile.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		logFile.Close()
		if t.Failed() {
			b, _ := os.ReadFile(logFile.Name())
			t.Log(string(b))
		}
	})
	a := NewAria2(rpcPort, "test-integration-secret")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = a.Call(ctx, "getVersion", nil, nil)
		cancel()
		if err == nil {
			return a
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("aria2 did not become ready")
	return nil
}

func startRealAriaSession(t *testing.T, binary, dir, session string, btPort int) (*exec.Cmd, *Aria2) {
	t.Helper()
	rpcPort := freePort(t)
	logFile, err := os.Create(filepath.Join(dir, "session-engine.log"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--no-conf=true", "--enable-rpc=true", "--rpc-listen-all=false",
		fmt.Sprintf("--rpc-listen-port=%d", rpcPort), "--rpc-secret=session-test-secret",
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
		_ = logFile.Close()
		t.Fatal(err)
	}
	_ = logFile.Close()
	a := NewAria2(rpcPort, "session-test-secret")
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = a.Call(ctx, "getVersion", nil, nil)
		cancel()
		if err == nil {
			return cmd, a
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	t.Fatalf("aria2 did not become ready")
	return nil, nil
}

func stopRealAriaSession(t *testing.T, cmd *exec.Cmd, a *Aria2) {
	t.Helper()
	if cmd == nil || cmd.ProcessState != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = a.Call(ctx, "shutdown", nil, nil)
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

func waitPayload(t *testing.T, path string, payload []byte, a *Aria2) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && bytes.Equal(b, payload) {
			return
		}
		items, err := a.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range items {
			if i.Status == "error" {
				t.Fatal(i.Error)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("payload did not complete:", path)
}
func TestRealAria2HTTP(t *testing.T) {
	if os.Getenv("WIRECTL_TEST_ARIA2") == "" {
		t.Skip("real engine not configured")
	}
	payload := bytes.Repeat([]byte("wire-download integration fixture\n"), 8192)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "fixture.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()
	dir := t.TempDir()
	a := realAria(t, dir, freePort(t))
	if _, err := a.Add(context.Background(), server.URL+"/fixture.bin"); err != nil {
		t.Fatal(err)
	}
	waitPayload(t, filepath.Join(dir, "fixture.bin"), payload, a)
}

func TestRealAria2ForgetMissingGID(t *testing.T) {
	if os.Getenv("WIRECTL_TEST_ARIA2") == "" {
		t.Skip("real engine not configured")
	}
	a := realAria(t, t.TempDir(), freePort(t))
	if err := a.Forget(context.Background(), "1234567890abcdef"); err != nil {
		t.Fatal(err)
	}
}

func TestRealAria2PauseSessionRestore(t *testing.T) {
	binaryPath := os.Getenv("WIRECTL_TEST_ARIA2")
	if binaryPath == "" {
		t.Skip("real engine not configured")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "downloads")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "session.txt")
	if err := os.WriteFile(session, nil, 0600); err != nil {
		t.Fatal(err)
	}
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := bencode(map[string]any{"interval": 1, "complete": 0, "incomplete": 1, "peers": []byte{}})
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer tracker.Close()
	info := map[string]any{
		"name":         "paused-fixture.bin",
		"length":       1,
		"piece length": 16384,
		"pieces":       func() []byte { sum := sha1.Sum([]byte{0}); return sum[:] }(),
	}
	torrent := bencode(map[string]any{"announce": tracker.URL, "info": info})
	torrentPath := filepath.Join(root, "paused-fixture.torrent")
	if err := os.WriteFile(torrentPath, torrent, 0600); err != nil {
		t.Fatal(err)
	}

	cmd, a := startRealAriaSession(t, binaryPath, dir, session, freePort(t))
	defer stopRealAriaSession(t, cmd, a)
	gid, err := a.Add(context.Background(), torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	ready := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		var status ariaStatus
		err = a.Call(context.Background(), "tellStatus", []any{gid}, &status)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "waiting" || status.Status == "active" {
			ready = true
			break
		}
		if isTerminalAriaStatus(status.Status) {
			t.Fatalf("torrent reached terminal state before pause: %#v", status)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("torrent did not reach a pausable state")
	}
	if err := a.Pause(context.Background(), gid); err != nil {
		stopRealAriaSession(t, cmd, a)
		t.Fatal(err)
	}
	var paused ariaStatus
	if err := a.Call(context.Background(), "tellStatus", []any{gid}, &paused); err != nil {
		stopRealAriaSession(t, cmd, a)
		t.Fatal(err)
	}
	if paused.Status != "paused" {
		stopRealAriaSession(t, cmd, a)
		t.Fatalf("Pause returned before native paused state: %#v", paused)
	}
	if err := a.SaveSession(context.Background()); err != nil {
		stopRealAriaSession(t, cmd, a)
		t.Fatal(err)
	}
	saved, err := os.ReadFile(session)
	if err != nil {
		stopRealAriaSession(t, cmd, a)
		t.Fatal(err)
	}
	if !bytes.Contains(saved, []byte("pause=true")) {
		stopRealAriaSession(t, cmd, a)
		t.Fatalf("paused task was not serialized with pause=true:\n%s", saved)
	}
	stopRealAriaSession(t, cmd, a)

	restartedCmd, restarted := startRealAriaSession(t, binaryPath, dir, session, freePort(t))
	defer stopRealAriaSession(t, restartedCmd, restarted)
	var restored ariaStatus
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		err = restarted.Call(context.Background(), "tellStatus", []any{gid}, &restored)
		if err == nil && restored.Status == "paused" {
			return
		}
		if err == nil && isTerminalAriaStatus(restored.Status) {
			t.Fatalf("paused task resumed into terminal state after restart: %#v", restored)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("paused task did not restore as paused: status=%#v err=%v", restored, err)
}

func TestRealAria2RemoveHTTPMetadataParent(t *testing.T) {
	if os.Getenv("WIRECTL_TEST_ARIA2") == "" {
		t.Skip("real engine not configured")
	}
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := bencode(map[string]any{"interval": 1, "complete": 0, "incomplete": 1, "peers": []byte{}})
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer tracker.Close()
	info := map[string]any{
		"name":         "metadata-child.bin",
		"length":       1,
		"piece length": 16384,
		"pieces":       func() []byte { sum := sha1.Sum([]byte{0}); return sum[:] }(),
	}
	torrent := bencode(map[string]any{"announce": tracker.URL, "info": info})
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fixture.torrent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Header().Set("Content-Length", strconv.Itoa(len(torrent)))
		_, _ = w.Write(torrent)
	}))
	defer fixture.Close()

	a := realAria(t, t.TempDir(), freePort(t))
	parentID, err := a.Add(context.Background(), fixture.URL+"/fixture.torrent")
	if err != nil {
		t.Fatal(err)
	}
	var followed []string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		items, listErr := a.List(context.Background())
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, item := range items {
			if item.ID == parentID && len(item.FollowedBy) > 0 {
				followed = append([]string(nil), item.FollowedBy...)
				break
			}
		}
		if len(followed) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(followed) == 0 {
		items, _ := a.List(context.Background())
		t.Fatalf("HTTP torrent metadata parent did not expose a child: parent=%s items=%#v", parentID, items)
	}
	if err := a.Remove(context.Background(), parentID); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		items, listErr := a.List(context.Background())
		if listErr != nil {
			t.Fatal(listErr)
		}
		present := map[string]bool{parentID: true}
		for _, id := range followed {
			present[id] = true
		}
		found := false
		for _, item := range items {
			if present[item.ID] {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	items, _ := a.List(context.Background())
	t.Fatalf("metadata parent removal left parent or child: %#v", items)
}

func bencode(v any) []byte {
	switch x := v.(type) {
	case string:
		return append([]byte(strconv.Itoa(len(x))+":"), []byte(x)...)
	case []byte:
		return append([]byte(strconv.Itoa(len(x))+":"), x...)
	case int:
		return []byte(fmt.Sprintf("i%de", x))
	case map[string]any:
		out := []byte{'d'}
		keys := []string{}
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, bencode(k)...)
			out = append(out, bencode(x[k])...)
		}
		return append(out, 'e')
	}
	panic("unsupported bencode type")
}
func TestRealAria2TorrentAndMagnet(t *testing.T) {
	if os.Getenv("WIRECTL_TEST_ARIA2") == "" {
		t.Skip("real engine not configured")
	}
	payload := bytes.Repeat([]byte("Locally generated legal BitTorrent test data.\n"), 4096)
	seedPort := freePort(t)
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peers := []byte{127, 0, 0, 1, 0, 0}
		binary.BigEndian.PutUint16(peers[4:], uint16(seedPort))
		w.Write(bencode(map[string]any{"interval": 1, "complete": 1, "incomplete": 1, "peers": peers}))
	}))
	defer tracker.Close()
	var pieces []byte
	for offset := 0; offset < len(payload); offset += 16384 {
		sum := sha1.Sum(payload[offset:min(offset+16384, len(payload))])
		pieces = append(pieces, sum[:]...)
	}
	info := map[string]any{"name": "fixture.bin", "length": len(payload), "piece length": 16384, "pieces": pieces}
	torrent := bencode(map[string]any{"announce": tracker.URL, "info": info})
	hash := sha1.Sum(bencode(info))
	seedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(seedDir, "fixture.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	torrentPath := filepath.Join(seedDir, "fixture.torrent")
	if err := os.WriteFile(torrentPath, torrent, 0600); err != nil {
		t.Fatal(err)
	}
	seed := realAria(t, seedDir, seedPort)
	if _, err := seed.Add(context.Background(), torrentPath); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"torrent", "magnet"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			leecher := realAria(t, dir, freePort(t))
			source := torrentPath
			if mode == "magnet" {
				source = "magnet:?xt=urn:btih:" + hex.EncodeToString(hash[:]) + "&tr=" + url.QueryEscape(tracker.URL)
			}
			if _, err := leecher.Add(context.Background(), source); err != nil {
				t.Fatal(err)
			}
			waitPayload(t, filepath.Join(dir, "fixture.bin"), payload, leecher)
		})
	}
}
