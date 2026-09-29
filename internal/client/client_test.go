package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/k0ngk0ng/wire-download/internal/daemon"
	"github.com/k0ngk0ng/wire-download/internal/engine"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &Client{http: &http.Client{Transport: transport}}
}

func TestSubmitOrdinarySourcesAgainstLegacyAndCurrentDaemon(t *testing.T) {
	sources := []string{
		"http://example.test/fixture.bin",
		"https://iso.omarchy.org/omarchy-4.0.4.iso",
		"https://example.test/fixture.torrent?token=abc",
		"https://video.twimg.com/fixture.mp4",
		"magnet:?xt=urn:btih:0123456789012345678901234567890123456789",
		"ed2k://|file|fixture.bin|42|01234567890123456789012345678901|/",
		"/downloads/fixture.torrent",
	}
	for _, current := range []bool{false, true} {
		name := "legacy"
		if current {
			name = "current"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body struct{ Source string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(daemon.Job{ID: "legacy-job", Source: body.Source, Engine: "aria2"})
			})
			if current {
				mux.HandleFunc("POST /v1/submissions", func(w http.ResponseWriter, r *http.Request) {
					t.Error("ordinary download used the new video API")
					http.Error(w, "unexpected video request", 500)
				})
			}
			c := testClient(t, mux)
			for _, source := range sources {
				result, err := c.Submit(context.Background(), source)
				if err != nil || len(result.Jobs) != 1 || result.Jobs[0].ID != "legacy-job" || result.Jobs[0].Source != source {
					t.Fatalf("Submit(%q) = %+v, %v", source, result, err)
				}
			}
			if calls.Load() != int32(len(sources)) {
				t.Fatalf("expected exactly one request per source; got %d", calls.Load())
			}
		})
	}
}

func TestSubmitVideoPreservesBatchAndPartialErrors(t *testing.T) {
	want := daemon.Submission{
		Jobs:   []daemon.Job{{ID: "first", Engine: "yt-dlp", Item: engine.Item{Name: "first video"}}, {ID: "second", Engine: "yt-dlp"}},
		Errors: []string{"third video unavailable"},
	}
	for _, source := range []string{"https://x.com/a/status/123/video/1?s=46", "https://twitter.com/a/status/123", "https://youtu.be/jNQXAC9IVRw"} {
		t.Run(source, func(t *testing.T) {
			c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/submissions" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var body struct{ Source string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Source != source {
					t.Errorf("source changed: %+v, %v", body, err)
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(want)
			}))
			got, err := c.Submit(context.Background(), source)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestSubmitVideoToLegacyDaemonExplainsRecoveryWithoutHTMLFallback(t *testing.T) {
	for _, code := range []int{404, 405} {
		var calls atomic.Int32
		c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path != "/v1/submissions" {
				t.Errorf("video fell back to %s", r.URL.Path)
			}
			http.Error(w, http.StatusText(code), code)
		}))
		_, err := c.Submit(context.Background(), "https://x.com/a/status/123")
		if err == nil || !strings.Contains(err.Error(), "upgrade") || !strings.Contains(err.Error(), "daemon restart") || !strings.Contains(err.Error(), "/v1/submissions") {
			t.Fatalf("missing recovery instructions: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("submission retried %d times", calls.Load())
		}
	}
}

func TestSubmitInvalidVideoPagesNeverContactDaemon(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid video caused a request: %s", r.URL.Path)
		http.Error(w, "unexpected", 500)
	}))
	for _, source := range []string{"https://x.com/home", "https://youtube.com/playlist?list=abc", "https://youtu.be/invalid", "https://x.com:443/a/status/123"} {
		if _, err := c.Submit(context.Background(), source); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
}

func TestSubmitPreservesServerErrorsWithoutRetry(t *testing.T) {
	for _, source := range []string{"https://example.test/file", "https://x.com/a/status/123"} {
		for _, tc := range []struct {
			code int
			body string
			want string
		}{
			{422, `{"error":"engine unavailable"}`, "engine unavailable"},
			{500, `{"error":"persist task: disk full"}`, "persist task: disk full"},
			{503, "unavailable", "HTTP 503 Service Unavailable"},
			{500, `{}`, "HTTP 500 Internal Server Error"},
			{500, `{"error":`, "HTTP 500 Internal Server Error"},
		} {
			var calls atomic.Int32
			c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			_, err := c.Submit(context.Background(), source)
			var response *HTTPError
			if !errors.As(err, &response) || response.StatusCode != tc.code || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("lost server error: %v", err)
			}
			if calls.Load() != 1 || strings.Contains(err.Error(), "upgrade") {
				t.Fatalf("unexpected retry or version diagnosis: calls=%d, err=%v", calls.Load(), err)
			}
		}
	}
}
