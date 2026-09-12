// Package auth stores browser sessions privately and scopes them to a website.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
}

type Session struct {
	Origin    string    `json:"origin"`
	UserAgent string    `json:"user_agent"`
	Cookies   []Cookie  `json:"cookies"`
	Saved     time.Time `json:"saved"`
}

func Origin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("login needs an HTTP(S) website URL without embedded credentials")
	}
	u.Host = strings.ToLower(u.Host)
	return u.Scheme + "://" + u.Host, nil
}

func (s Session) Validate() error {
	origin, err := Origin(s.Origin)
	if err != nil || origin != s.Origin {
		return errors.New("invalid session origin")
	}
	if strings.ContainsAny(s.UserAgent, "\r\n\x00") || len(s.UserAgent) > 4096 {
		return errors.New("invalid browser user agent")
	}
	u, _ := url.Parse(origin)
	if len(s.Cookies) == 0 || len(s.Cookies) > 4096 {
		return errors.New("no login cookies found for this website")
	}
	for _, c := range s.Cookies {
		domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
		if domain == "" || (domain != u.Hostname() && !strings.HasSuffix(u.Hostname(), "."+domain)) {
			return errors.New("session contains cookies for another website")
		}
		if strings.HasPrefix(c.Domain, ".") {
			if suffix, _ := publicsuffix.PublicSuffix(domain); suffix == domain {
				return errors.New("invalid cookie domain")
			}
		}
		if err := (&http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: domain}).Valid(); err != nil {
			return errors.New("invalid browser cookie")
		}
	}
	return nil
}

func sessionPath(dir, origin string) string {
	hash := sha256.Sum256([]byte(origin))
	return filepath.Join(dir, "auth", hex.EncodeToString(hash[:])+".json")
}

func Save(dir string, s Session) error {
	if err := s.Validate(); err != nil {
		return err
	}
	s.Saved = time.Now().UTC()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return errors.New("browser session is too large")
	}
	path := sessionPath(dir, s.Origin)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func Load(dir, rawURL string) (*Session, error) {
	origin, err := Origin(rawURL)
	if err != nil {
		return nil, nil
	}
	path := sessionPath(dir, origin)
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 1<<20 || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("login session must be a private file (mode 600)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if json.Unmarshal(b, &s) != nil || s.Origin != origin {
		return nil, errors.New("invalid saved login session")
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func Forget(dir, rawURL string) error {
	origin, err := Origin(rawURL)
	if err != nil {
		return err
	}
	err = os.Remove(sessionPath(dir, origin))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s Session) Jar() (http.CookieJar, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	u, _ := url.Parse(s.Origin)
	for _, c := range s.Cookies {
		domain := c.Domain
		if !strings.HasPrefix(domain, ".") {
			domain = ""
		} // Chrome's host-only cookies
		cookie := &http.Cookie{Name: c.Name, Value: c.Value, Domain: domain, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
		if c.Expires > 0 {
			cookie.Expires = time.Unix(int64(c.Expires), 0)
		}
		jar.SetCookies(u, []*http.Cookie{cookie})
	}
	return jar, nil
}

func Describe(s Session) string {
	return fmt.Sprintf("Saved login for %s (%d cookies)", s.Origin, len(s.Cookies))
}
