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
