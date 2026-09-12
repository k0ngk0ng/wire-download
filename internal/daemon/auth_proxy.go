package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/auth"
	"github.com/k0ngk0ng/wire-download/internal/config"
)

// Cookies stay in the Go HTTP client's domain-aware jar. aria2 receives only
// a private loopback URL, so redirects cannot forward a raw Cookie header to
// another website and engine session files never contain browser cookies.
type authProxy struct {
	dir, secret string
	port        int
	store       *Store
}

func (p *authProxy) token(id string) string {
	mac := hmac.New(sha256.New, []byte(p.secret))
	_, _ = mac.Write([]byte("download-session:" + id))
	return hex.EncodeToString(mac.Sum(nil))
}
func (p *authProxy) source(source, id string) (string, error) {
	session, err := auth.Load(p.dir, source)
	if err != nil {
		return "", err
	}
	if session == nil {
		return source, nil
	}
	return fmt.Sprintf("http://127.0.0.1:%d/fetch/%s/%s", p.port, id, p.token(id)), nil
}
func (p *authProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if (r.Method != "GET" && r.Method != "HEAD") || len(parts) != 3 || parts[0] != "fetch" || !hmac.Equal([]byte(parts[2]), []byte(p.token(parts[1]))) || r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var source string
	p.store.mu.Lock()
	for _, j := range p.store.state.Jobs {
		if j.Engine == "aria2" && j.Status != "removed" && (j.Item.ID == parts[1] || j.metadataRootID() == parts[1]) {
			source = j.Source
			break
		}
	}
	p.store.mu.Unlock()
	if source == "" {
		http.NotFound(w, r)
		return
	}
	session, err := auth.Load(p.dir, source)
	if err != nil || session == nil {
		http.Error(w, "login session unavailable; log in again", http.StatusUnauthorized)
		return
	}
	jar, err := session.Jar()
	if err != nil {
		http.Error(w, "invalid login session", http.StatusUnauthorized)
		return
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	defer transport.CloseIdleConnections()
	if cert := config.FallbackCAFile(); cert != "" {
		pem, readErr := os.ReadFile(cert)
		roots := x509.NewCertPool()
		if readErr != nil || !roots.AppendCertsFromPEM(pem) {
			http.Error(w, "invalid CA bundle", 500)
			return
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{Transport: transport, Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("unsupported redirect")
		}
		if req.URL.Scheme == "http" && via[len(via)-1].URL.Scheme == "https" {
			return errors.New("HTTPS download redirected to HTTP")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, source, nil)
	if err != nil {
		http.Error(w, "invalid download URL", 400)
		return
	}
	req.Header.Set("User-Agent", session.UserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	for _, name := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match"} {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	response, err := client.Do(req)
	if err != nil {
		http.Error(w, "authenticated request failed; check website access and login", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", "Content-Disposition", "Content-Encoding"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if w.Header().Get("Content-Disposition") == "" {
		u, _ := url.Parse(source)
		name := path.Base(u.Path)
		if name == "." || name == "/" {
			name = "index.html"
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	w.WriteHeader(response.StatusCode)
	if r.Method != "HEAD" {
		_, _ = io.Copy(w, response.Body)
	}
}

func startAuthProxy(ctx context.Context, dir string, c config.Config, store *Store) (func(), error) {
	p := &authProxy{dir: dir, secret: c.Secret, port: c.AuthProxyPort, store: store}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.port))
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: p, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return ctx }}
	store.proxySource = p.source
	go func() { _ = server.Serve(ln) }()
	return func() { _ = server.Close() }, nil
}
