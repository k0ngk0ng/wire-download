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
	"sync"
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

const (
	maxServerMetBytes             = 4 << 20
	maxMergedServerMetBytes       = 8 << 20
	maxServerMetRecords           = 10000
	maxServerMetSources           = 8
	maxConcurrentServerMetFetches = 4
	serverMetRequestTimeout       = 8 * time.Second
	serverMetTotalDeadline        = 15 * time.Second
)

type serverMetRecord struct {
	key [6]byte
	raw []byte
}

type parsedServerMet struct {
	version byte
	records []serverMetRecord
}

// ValidateServerMet checks structure, lengths and supported tag types before an
// untrusted server list is passed to aMule. Unknown formats are rejected.
func ValidateServerMet(b []byte) error {
	_, err := parseServerMet(b)
	return err
}

func parseServerMet(b []byte) (parsedServerMet, error) {
	if len(b) < 5 || (b[0] != 0xe0 && b[0] != 0x0e) {
		return parsedServerMet{}, errors.New("invalid server.met header")
	}
	n := int(binary.LittleEndian.Uint32(b[1:5]))
	if n == 0 || n > maxServerMetRecords {
		return parsedServerMet{}, errors.New("invalid server count")
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
	records := make([]serverMetRecord, 0, n)
	for i := 0; i < n; i++ {
		start := pos
		h, err := take(10)
		if err != nil {
			return parsedServerMet{}, err
		}
		if binary.LittleEndian.Uint16(h[4:6]) == 0 {
			return parsedServerMet{}, errors.New("server port is zero")
		}
		tagCount := binary.LittleEndian.Uint32(h[6:10])
		if tagCount > 1000 {
			return parsedServerMet{}, errors.New("too many server tags")
		}
		tags := int(tagCount)
		for j := 0; j < tags; j++ {
			typ, err := take(1)
			if err != nil {
				return parsedServerMet{}, err
			}
			t := typ[0]
			if t&0x80 != 0 {
				if _, err = take(1); err != nil {
					return parsedServerMet{}, err
				}
				t &= 0x7f
			} else {
				l, err := take(2)
				if err != nil {
					return parsedServerMet{}, err
				}
				if _, err = take(int(binary.LittleEndian.Uint16(l))); err != nil {
					return parsedServerMet{}, err
				}
			}
			var size int
			switch {
			case t == 1:
				size = 16
			case t == 2:
				l, err := take(2)
				if err != nil {
					return parsedServerMet{}, err
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
				return parsedServerMet{}, fmt.Errorf("unsupported server tag %x", t)
			}
			if _, err = take(size); err != nil {
				return parsedServerMet{}, err
			}
		}
		var key [6]byte
		copy(key[:], b[start:start+6])
		records = append(records, serverMetRecord{key: key, raw: append([]byte(nil), b[start:pos]...)})
	}
	if pos != len(b) {
		return parsedServerMet{}, errors.New("trailing server.met data")
	}
	return parsedServerMet{version: b[0], records: records}, nil
}

func mergeServerMets(lists [][]byte) ([]byte, error) {
	if len(lists) == 0 {
		return nil, errors.New("no valid server.met lists")
	}
	seen := make(map[[6]byte]struct{})
	records := make([]serverMetRecord, 0)
	var version byte
	totalBytes := 5
	for i, list := range lists {
		parsed, err := parseServerMet(list)
		if err != nil {
			return nil, fmt.Errorf("server.met source %d: %w", i+1, err)
		}
		if i == 0 {
			version = parsed.version
		}
		for _, record := range parsed.records {
			if _, ok := seen[record.key]; ok {
				continue
			}
			if len(records) >= maxServerMetRecords {
				return nil, fmt.Errorf("merged server count exceeds %d", maxServerMetRecords)
			}
			if totalBytes > maxMergedServerMetBytes-len(record.raw) {
				return nil, fmt.Errorf("merged server.met exceeds %d bytes", maxMergedServerMetBytes)
			}
			seen[record.key] = struct{}{}
			records = append(records, record)
			totalBytes += len(record.raw)
		}
	}
	if len(records) == 0 {
		return nil, errors.New("no server records in merged server.met")
	}
	out := make([]byte, 5, totalBytes)
	out[0] = version
	binary.LittleEndian.PutUint32(out[1:5], uint32(len(records)))
	for _, record := range records {
		out = append(out, record.raw...)
	}
	return out, nil
}

func newServerMetHTTPClient() (*http.Client, func(), error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Timeout: serverMetRequestTimeout, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
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
			transport.CloseIdleConnections()
			return nil, nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			transport.CloseIdleConnections()
			return nil, nil, errors.New("bundled CA certificates are invalid")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return client, transport.CloseIdleConnections, nil
}

type serverMetFetchResult struct {
	index  int
	source string
	data   []byte
	err    error
}

func uniqueServerMetSources(sources []string) []string {
	seen := make(map[string]struct{}, len(sources))
	unique := make([]string, 0, min(len(sources), maxServerMetSources))
	for _, source := range sources {
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		if len(unique) == maxServerMetSources {
			break
		}
		unique = append(unique, source)
	}
	return unique
}

func fetchServerMet(ctx context.Context, client *http.Client, source string) ([]byte, error) {
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("server list source must be an HTTPS URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET server.met: %w", err)
	}
	defer res.Body.Close()
	if res.ContentLength > maxServerMetBytes {
		return nil, fmt.Errorf("server.met response exceeds %d bytes", maxServerMetBytes)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server.met HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxServerMetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read server.met: %w", err)
	}
	if len(b) > maxServerMetBytes {
		return nil, fmt.Errorf("server.met response exceeds %d bytes", maxServerMetBytes)
	}
	if err := ValidateServerMet(b); err != nil {
		return nil, fmt.Errorf("invalid server.met: %w", err)
	}
	return b, nil
}

func fetchServerMets(ctx context.Context, client *http.Client, sources []string) []serverMetFetchResult {
	sources = uniqueServerMetSources(sources)
	if len(sources) == 0 {
		return nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, serverMetTotalDeadline)
	defer cancel()
	sem := make(chan struct{}, maxConcurrentServerMetFetches)
	results := make(chan serverMetFetchResult, len(sources))
	var wg sync.WaitGroup
	for i, source := range sources {
		wg.Add(1)
		go func(index int, source string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-fetchCtx.Done():
				results <- serverMetFetchResult{index: index, source: source, err: fetchCtx.Err()}
				return
			}
			defer func() { <-sem }()
			data, err := fetchServerMet(fetchCtx, client, source)
			results <- serverMetFetchResult{index: index, source: source, data: data, err: err}
		}(i, source)
	}
	wg.Wait()
	close(results)
	ordered := make([]serverMetFetchResult, len(sources))
	for result := range results {
		ordered[result.index] = result
	}
	return ordered
}

func allServerMetFetchesFailed(results []serverMetFetchResult) error {
	if len(results) == 0 {
		return errors.New("no server list sources configured")
	}
	failures := make([]error, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", result.source, result.err))
		}
	}
	if len(failures) == 0 {
		return errors.New("no server list source succeeded")
	}
	return fmt.Errorf("all server list sources failed: %w", errors.Join(failures...))
}

func BootstrapServers(ctx context.Context, dir string, urls []string, force bool) error {
	client, closeClient, err := newServerMetHTTPClient()
	if err != nil {
		return err
	}
	defer closeClient()
	return bootstrapServersWithClient(ctx, dir, urls, force, client)
}

func bootstrapServersWithClient(ctx context.Context, dir string, urls []string, force bool, client *http.Client) error {
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
	if client == nil {
		return errors.New("server list HTTP client is nil")
	}
	sources := uniqueServerMetSources(urls)
	if len(urls) > len(sources) {
		log.Printf("server list source limit: using %d of %d configured sources", len(sources), len(urls))
	}
	results := fetchServerMets(ctx, client, sources)
	valid := make([][]byte, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			log.Printf("server list source %s failed: %v", result.source, result.err)
			continue
		}
		valid = append(valid, result.data)
	}
	if len(valid) == 0 {
		last := allServerMetFetchesFailed(results)
		if !force && ValidateServerMet(bundledServers) == nil {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			log.Printf("server list refresh unavailable (%v); using bundled bootstrap list", last)
			return atomicWrite(path, bundledServers)
		}
		return fmt.Errorf("cannot bootstrap eMule servers: %w", last)
	}
	merged, err := mergeServerMets(valid)
	if err != nil {
		return fmt.Errorf("merge server lists: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicWrite(path, merged)
}
