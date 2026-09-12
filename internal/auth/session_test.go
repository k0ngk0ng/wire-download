package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSessionScopesCookiesAndPreservesPrivacy(t *testing.T) {
	s := Session{Origin: "https://files.example.com", UserAgent: "browser", Cookies: []Cookie{{Name: "sid", Value: "private-value", Domain: ".example.com", Path: "/private", Secure: true, HTTPOnly: true}}}
	dir := t.TempDir()
	if err := Save(dir, s); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(sessionPath(dir, s.Origin))
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	loaded, err := Load(dir, s.Origin+"/private/file")
	if err != nil {
		t.Fatal(err)
	}
	jar, err := loaded.Jar()
	if err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]int{"https://files.example.com/private/file": 1, "https://other.invalid/private/file": 0, "http://files.example.com/private/file": 0, "https://files.example.com/public": 0} {
		u, _ := url.Parse(target)
		if got := len(jar.Cookies(u)); got != want {
			t.Fatalf("%s cookies=%d want=%d", target, got, want)
		}
	}
	s.Cookies[0].Domain = ".com"
	if Save(dir, s) == nil {
		t.Fatal("accepted public-suffix cookie")
	}
	if err = Forget(dir, loaded.Origin); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(dir, loaded.Origin); got != nil {
		t.Fatal("logout left session")
	}
}

func TestRealBrowserCapturesHTTPOnlyCookie(t *testing.T) {
	binary := os.Getenv("WIRECTL_TEST_BROWSER")
	if binary == "" {
		t.Skip("set WIRECTL_TEST_BROWSER to an installed Chromium browser")
	}
	seen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "browser-fixture", Path: "/", HttpOnly: true})
		_, _ = w.Write([]byte("Owned login fixture"))
		select {
		case seen <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	s, err := Capture(ctx, t.TempDir(), server.URL, BrowserOptions{Binary: binary, Headless: true}, func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-seen:
		}
		time.Sleep(500 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cookies) != 1 || s.Cookies[0].Value != "browser-fixture" || !s.Cookies[0].HTTPOnly || !strings.Contains(s.UserAgent, "Chrome") {
		t.Fatal("browser session did not preserve the HttpOnly login cookie")
	}
}
