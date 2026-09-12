package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/k0ngk0ng/wire-download/internal/auth"
	"github.com/k0ngk0ng/wire-download/internal/engine"
)

func TestAuthenticatedProxyRangeAndRedirectIsolation(t *testing.T) {
	redirected := make(chan string, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected <- r.Header.Get("Cookie")
		_, _ = w.Write([]byte("public"))
	}))
	defer other.Close()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("sid")
		if err != nil || c.Value != "secret-cookie" {
			http.Error(w, "login required", 401)
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1), 302)
			return
		}
		if r.Header.Get("Range") != "bytes=2-4" {
			t.Errorf("range was not forwarded")
		}
		w.Header().Set("Content-Range", "bytes 2-4/6")
		w.Header().Set("Content-Length", "3")
		w.Header().Set("Set-Cookie", "rotated=value")
		w.WriteHeader(206)
		_, _ = w.Write([]byte("cde"))
	}))
	defer remote.Close()
	dir := t.TempDir()
	u, _ := url.Parse(remote.URL)
	if err := auth.Save(dir, auth.Session{Origin: remote.URL, UserAgent: "browser", Cookies: []auth.Cookie{{Name: "sid", Value: "secret-cookie", Domain: u.Hostname(), Path: "/"}}}); err != nil {
		t.Fatal(err)
	}
	store := &Store{state: State{Jobs: []Job{{ID: "public", Engine: "aria2", Source: remote.URL + "/file", Item: engine.Item{ID: "engine-id"}}}}}
	proxy := &authProxy{dir: dir, secret: "private-proxy-key", store: store}
	server := httptest.NewServer(proxy)
	defer server.Close()
	for _, source := range []string{"/file", "/redirect"} {
		store.mu.Lock()
		store.state.Jobs[0].Source = remote.URL + source
		store.mu.Unlock()
		req, _ := http.NewRequest("GET", server.URL+"/fetch/engine-id/"+proxy.token("engine-id"), nil)
		req.Header.Set("Range", "bytes=2-4")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if source == "/file" && (res.StatusCode != 206 || string(b) != "cde" || res.Header.Get("Content-Range") != "bytes 2-4/6") {
			t.Fatalf("invalid partial response %d %s", res.StatusCode, b)
		}
		if res.Header.Get("Set-Cookie") != "" {
			t.Fatal("origin cookie leaked into engine response")
		}
	}
	if cookie := <-redirected; cookie != "" {
		t.Fatal("login cookie leaked across hosts")
	}
	res, err := http.Get(server.URL + "/fetch/engine-id/wrong-token")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("invalid proxy token accepted")
	}
}

func TestAuthenticatedMetadataProxySurvivesChildReplacement(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("sid")
		if err != nil || cookie.Value != "owned-session" {
			http.Error(w, "login required", 401)
			return
		}
		_, _ = w.Write([]byte("owned torrent metadata"))
	}))
	defer remote.Close()
	dir := t.TempDir()
	u, _ := url.Parse(remote.URL)
	if err := auth.Save(dir, auth.Session{Origin: remote.URL, Cookies: []auth.Cookie{{Name: "sid", Value: "owned-session", Domain: u.Hostname(), Path: "/"}}}); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []bool{false, true} {
		job := Job{ID: "logical", Engine: "aria2", Source: remote.URL + "/fixture.torrent", Item: engine.Item{ID: "new-child", Status: "paused"}}
		parent := job.metadataRootID()
		if !legacy {
			job.MetadataID = parent
		}
		store := &Store{state: State{Jobs: []Job{job}}}
		proxy := &authProxy{dir: dir, secret: "fixture-proxy-key", store: store}
		path := "/fetch/" + parent + "/" + proxy.token(parent)
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || response.Body.String() != "owned torrent metadata" {
			t.Fatalf("legacy=%v metadata unavailable after handoff: %d %s", legacy, response.Code, response.Body.String())
		}
		store.state.Jobs[0].Status = "removed"
		response = httptest.NewRecorder()
		proxy.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 404 {
			t.Fatalf("removed task still available: %d", response.Code)
		}
	}
}
