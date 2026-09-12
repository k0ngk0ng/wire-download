package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/config"
)

// Bundled public server list from https://upd.emule-security.org/server.met,
// retrieved 2026-09-12. Live HTTPS refresh is preferred on first startup.
//
//go:embed servers.met
var bundledServers []byte

// Kad bootstrap contacts from the same source and retrieval date.
//
//go:embed nodes.dat
var bundledNodes []byte

// ValidateServerMet checks structure, lengths and supported tag types before an
// untrusted server list is passed to aMule. Unknown formats are rejected.
func ValidateServerMet(b []byte) error {
	if len(b) < 5 || (b[0] != 0xe0 && b[0] != 0x0e) {
		return errors.New("invalid server.met header")
	}
	n := int(binary.LittleEndian.Uint32(b[1:5]))
	if n == 0 || n > 10000 {
		return errors.New("invalid server count")
	}
	pos := 5
	take := func(n int) ([]byte, error) {
		if n < 0 || pos+n > len(b) {
			return nil, io.ErrUnexpectedEOF
		}
		v := b[pos : pos+n]
		pos += n
		return v, nil
	}
	for i := 0; i < n; i++ {
		h, err := take(10)
		if err != nil {
			return err
		}
		if binary.LittleEndian.Uint16(h[4:6]) == 0 {
			return errors.New("server port is zero")
		}
		tags := int(binary.LittleEndian.Uint32(h[6:10]))
		if tags > 1000 {
			return errors.New("too many server tags")
		}
		for j := 0; j < tags; j++ {
			typ, err := take(1)
			if err != nil {
				return err
			}
			t := typ[0]
			if t&0x80 != 0 {
				if _, err = take(1); err != nil {
					return err
				}
				t &= 0x7f
			} else {
				l, err := take(2)
				if err != nil {
					return err
				}
				if _, err = take(int(binary.LittleEndian.Uint16(l))); err != nil {
					return err
				}
			}
			var size int
			switch {
			case t == 1:
				size = 16
			case t == 2:
				l, err := take(2)
				if err != nil {
					return err
				}
				size = int(binary.LittleEndian.Uint16(l))
			case t == 3 || t == 4:
				size = 4
			case t == 5 || t == 9:
				size = 1
			case t == 8:
				size = 2
			case t == 11:
				size = 8
			case t >= 0x11 && t <= 0x20:
				size = int(t - 0x10)
			default:
				return fmt.Errorf("unsupported server tag %x", t)
			}
			if _, err = take(size); err != nil {
				return err
			}
		}
	}
	if pos != len(b) {
		return errors.New("trailing server.met data")
	}
	return nil
}

func BootstrapServers(ctx context.Context, dir string, urls []string, force bool) error {
	nodesPath := filepath.Join(dir, "amule", "nodes.dat")
	if _, err := os.Stat(nodesPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(nodesPath), 0700); err != nil {
			return err
		}
		if err := atomicWrite(nodesPath, bundledNodes); err != nil {
			return err
		}
	}
	path := filepath.Join(dir, "amule", "server.met")
	if !force {
		if b, err := os.ReadFile(path); err == nil && ValidateServerMet(b) == nil {
			return nil
		}
	}
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("server list redirect must use HTTPS")
		}
		if len(via) > 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
	if cert := config.FallbackCAFile(); cert != "" {
		pem, err := os.ReadFile(cert)
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return errors.New("bundled CA certificates are invalid")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}
	var last error
	for _, source := range urls {
		u, err := url.Parse(source)
		if err != nil || u.Scheme != "https" {
			last = errors.New("server list must use HTTPS")
			continue
		}
		req, err := http.NewRequestWithContext(ctx, "GET", source, nil)
		if err != nil {
			last = err
			continue
		}
		res, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
		res.Body.Close()
		if err == nil && res.StatusCode != 200 {
			err = fmt.Errorf("HTTP %d", res.StatusCode)
		}
		if err == nil {
			err = ValidateServerMet(b)
		}
		if err != nil {
			last = err
			continue
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		return atomicWrite(path, b)
	}
	if !force && ValidateServerMet(bundledServers) == nil {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		log.Printf("server list refresh unavailable (%v); using bundled bootstrap list", last)
		return atomicWrite(path, bundledServers)
	}
	return fmt.Errorf("cannot bootstrap eMule servers: %w", last)
}
