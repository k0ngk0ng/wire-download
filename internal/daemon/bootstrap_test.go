package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerMetValidation(t *testing.T) {
	b := make([]byte, 15)
	b[0] = 0xe0
	binary.LittleEndian.PutUint32(b[1:5], 1)
	copy(b[5:9], []byte{1, 2, 3, 4})
	binary.LittleEndian.PutUint16(b[9:11], 4661)
	if err := ValidateServerMet(b); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(b); n++ {
		if err := ValidateServerMet(b[:n]); err == nil {
			t.Fatalf("truncation %d accepted", n)
		}
	}
	b = append(b, 0)
	if err := ValidateServerMet(b); err == nil {
		t.Fatal("trailing data accepted")
	}
}
func TestDownloadedServerMet(t *testing.T) {
	path := os.Getenv("WIRECTL_TEST_SERVER_MET")
	if path == "" {
		t.Skip("optional live downloaded server.met fixture")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateServerMet(b); err != nil {
		t.Fatal(err)
	}
}

type testServerMetEntry struct {
	ip   [4]byte
	port uint16
	tag  string
}

func makeTestServerMet(entries ...testServerMetEntry) []byte {
	out := make([]byte, 5)
	out[0] = 0xe0
	binary.LittleEndian.PutUint32(out[1:5], uint32(len(entries)))
	for _, entry := range entries {
		out = append(out, entry.ip[:]...)
		var port [2]byte
		binary.LittleEndian.PutUint16(port[:], entry.port)
		out = append(out, port[:]...)
		var tags [4]byte
		if entry.tag != "" {
			binary.LittleEndian.PutUint32(tags[:], 1)
		}
		out = append(out, tags[:]...)
		if entry.tag != "" {
			value := []byte(entry.tag)
			if len(value) > 65535 {
				panic("test server tag is too long")
			}
			out = append(out, 0x02, 0x01, 0x00, 0x01)
			var length [2]byte
			binary.LittleEndian.PutUint16(length[:], uint16(len(value)))
			out = append(out, length[:]...)
			out = append(out, value...)
		}
	}
	return out
}

func TestMergeServerMetsDeduplicatesAndPreservesFirstRecord(t *testing.T) {
	first := makeTestServerMet(
		testServerMetEntry{ip: [4]byte{1, 2, 3, 4}, port: 4661, tag: "first-tags"},
		testServerMetEntry{ip: [4]byte{5, 6, 7, 8}, port: 4662, tag: "second-tags"},
	)
	second := makeTestServerMet(
		testServerMetEntry{ip: [4]byte{1, 2, 3, 4}, port: 4661, tag: "duplicate-tags"},
		testServerMetEntry{ip: [4]byte{9, 10, 11, 12}, port: 4663, tag: "third-tags"},
	)
	merged, err := mergeServerMets([][]byte{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateServerMet(merged); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseServerMet(merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.records) != 3 {
		t.Fatalf("merged records=%d, want 3", len(parsed.records))
	}
	firstParsed, err := parseServerMet(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.records[0].raw, firstParsed.records[0].raw) {
		t.Fatal("duplicate server did not retain the first complete record and tags")
	}
	if bytes.Contains(parsed.records[0].raw, []byte("duplicate-tags")) {
		t.Fatal("duplicate server retained the later record tags")
	}
}

func TestBootstrapServersMergesValidHTTPSListsAndKeepsPartialFailures(t *testing.T) {
	first := makeTestServerMet(testServerMetEntry{ip: [4]byte{10, 0, 0, 1}, port: 4661, tag: "first"})
	bad := []byte{0xe0, 1, 0, 0, 0}
	third := makeTestServerMet(testServerMetEntry{ip: [4]byte{10, 0, 0, 2}, port: 4662, tag: "third"})
	var mu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		var body []byte
		switch r.URL.Path {
		case "/first":
			body = first
		case "/bad":
			body = bad
		case "/third":
			body = third
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", stringLength(len(body)))
		_, _ = w.Write(body)
	}))
	defer server.Close()

	dir := t.TempDir()
	err := bootstrapServersWithClient(
		context.Background(),
		dir,
		[]string{server.URL + "/first", server.URL + "/bad", server.URL + "/third"},
		true,
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dir + "/amule/server.met")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseServerMet(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.records) != 2 {
		t.Fatalf("merged records=%d, want 2", len(parsed.records))
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["/first"] != 1 || calls["/bad"] != 1 || calls["/third"] != 1 {
		t.Fatalf("unexpected source calls: %#v", calls)
	}
}

func TestBootstrapServersKeepsValidFileWhenForceFetchFails(t *testing.T) {
	old := makeTestServerMet(testServerMetEntry{ip: [4]byte{192, 0, 2, 1}, port: 4661, tag: "old"})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/amule", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/amule/server.met", old, 0600); err != nil {
		t.Fatal(err)
	}
	nodes := []byte("keep kad state")
	if err := os.WriteFile(dir+"/amule/nodes.dat", nodes, 0600); err != nil {
		t.Fatal(err)
	}
	err := bootstrapServersWithClient(
		context.Background(),
		dir,
		[]string{server.URL + "/down", "http://127.0.0.1:1/server.met"},
		true,
		server.Client(),
	)
	if err == nil || !strings.Contains(err.Error(), "all server list sources failed") {
		t.Fatalf("force failure=%v, want explicit aggregate error", err)
	}
	got, err := os.ReadFile(dir + "/amule/server.met")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Fatal("force failure overwrote a previously valid server.met")
	}
	got, err = os.ReadFile(dir + "/amule/nodes.dat")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, nodes) {
		t.Fatal("server refresh changed existing Kad state")
	}
}

func TestBootstrapServersUsesBundledFallbackWhenNormalFetchFails(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	dir := t.TempDir()
	err := bootstrapServersWithClient(
		context.Background(),
		dir,
		[]string{server.URL + "/down"},
		false,
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dir + "/amule/server.met")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bundledServers) {
		t.Fatal("normal startup did not use the bundled server list fallback")
	}
}

func TestBootstrapServersDoesNotFetchWhenExistingFileIsValid(t *testing.T) {
	old := makeTestServerMet(testServerMetEntry{ip: [4]byte{198, 51, 100, 1}, port: 4661})
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write(makeTestServerMet(testServerMetEntry{ip: [4]byte{198, 51, 100, 2}, port: 4662}))
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/amule", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/amule/server.met", old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapServersWithClient(context.Background(), dir, []string{server.URL}, false, server.Client()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("valid existing server.met triggered %d network requests", got)
	}
	got, err := os.ReadFile(dir + "/amule/server.met")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Fatal("valid existing server.met was overwritten")
	}
}

func TestBootstrapServersBoundsSourcesAndConcurrency(t *testing.T) {
	var active, maximum atomic.Int32
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := active.Add(1)
		for {
			old := maximum.Load()
			if now <= old || maximum.CompareAndSwap(old, now) {
				break
			}
		}
		defer active.Add(-1)
		calls.Add(1)
		time.Sleep(25 * time.Millisecond)
		_, _ = w.Write(makeTestServerMet(testServerMetEntry{
			ip:   [4]byte{203, 0, 113, byte(calls.Load())},
			port: uint16(4661 + calls.Load()),
		}))
	}))
	defer server.Close()
	sources := make([]string, 0, maxServerMetSources+3)
	for i := 0; i < maxServerMetSources+3; i++ {
		sources = append(sources, server.URL+"/source-"+string(rune('a'+i)))
	}
	dir := t.TempDir()
	if err := bootstrapServersWithClient(context.Background(), dir, sources, true, server.Client()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != maxServerMetSources {
		t.Fatalf("fetched %d sources, want bounded %d", got, maxServerMetSources)
	}
	if got := maximum.Load(); got > maxConcurrentServerMetFetches {
		t.Fatalf("concurrency=%d, want <= %d", got, maxConcurrentServerMetFetches)
	}
}

func TestBootstrapServersHonorsCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	err := bootstrapServersWithClient(ctx, t.TempDir(), []string{server.URL}, true, server.Client())
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "context canceled") {
		t.Fatalf("cancellation error=%v", err)
	}
}

func stringLength(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var reversed [20]byte
	i := len(reversed)
	for n > 0 {
		i--
		reversed[i] = digits[n%10]
		n /= 10
	}
	return string(reversed[i:])
}

func FuzzServerMet(f *testing.F) {
	f.Add([]byte{0xe0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) { _ = ValidateServerMet(b) })
}
