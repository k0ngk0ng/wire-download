// Package search combines native and web indexes without downloading their results.
package search

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Request struct {
	Query          string   `json:"query"`
	Type           string   `json:"type,omitempty"`
	Sources        []string `json:"sources,omitempty"`
	Limit          int      `json:"limit,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	ED2KMode       string   `json:"ed2k_mode,omitempty"`
}

type Result struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Link       string   `json:"link"`
	TorrentURL string   `json:"torrent_url,omitempty"`
	InfoHash   string   `json:"info_hash,omitempty"`
	PageURL    string   `json:"page_url,omitempty"`
	Size       int64    `json:"size,omitempty"`
	Seeds      int64    `json:"seeds,omitempty"`
	Peers      int64    `json:"peers,omitempty"`
	Sources    []string `json:"sources"`
}

type SourceConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	URL     string `json:"url,omitempty"`
	Enabled bool   `json:"enabled"`
	APIKey  string `json:"api_key,omitempty"`
}

type SourceInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	URL     string `json:"url,omitempty"`
	Enabled bool   `json:"enabled"`
}

type Emit func(results []Result, progress int)
type Provider struct {
	Info   SourceInfo
	Search func(context.Context, Request, Emit) error
}

type SourceState struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Count    int    `json:"count"`
	Error    string `json:"error,omitempty"`
}

type Snapshot struct {
	ID        string        `json:"id"`
	Query     string        `json:"query"`
	Type      string        `json:"type"`
	Status    string        `json:"status"`
	Started   time.Time     `json:"started"`
	Updated   time.Time     `json:"updated"`
	Results   []Result      `json:"results"`
	Sources   []SourceState `json:"sources"`
	Total     int           `json:"total"`
	Truncated bool          `json:"truncated"`
}

func (s Snapshot) Done() bool {
	switch s.Status {
	case "complete", "partial", "failed", "cancelled":
		return true
	}
	return false
}

func (r *Request) Validate() error {
	r.Query = strings.TrimSpace(r.Query)
	if !utf8.ValidString(r.Query) || len(r.Query) == 0 || len(r.Query) > 1024 || strings.IndexFunc(r.Query, unicode.IsControl) >= 0 {
		return errors.New("query must be 1–1024 UTF-8 bytes without control characters")
	}
	if r.Type == "" {
		r.Type = "all"
	}
	switch r.Type {
	case "all", "ed2k", "bt", "magnet", "torrent":
	default:
		return errors.New("type must be all, ed2k, bt, magnet, or torrent")
	}
	if r.Limit == 0 {
		r.Limit = 50
	}
	if r.Limit < 1 || r.Limit > 200 {
		return errors.New("limit must be between 1 and 200")
	}
	if r.TimeoutSeconds == 0 {
		r.TimeoutSeconds = 60
	}
	if r.TimeoutSeconds < 5 || r.TimeoutSeconds > 180 {
		return errors.New("timeout must be between 5 and 180 seconds")
	}
	if r.ED2KMode == "" {
		r.ED2KMode = "all"
	}
	switch r.ED2KMode {
	case "all", "server", "global", "kad":
	default:
		return errors.New("ed2k mode must be all, server, global, or kad")
	}
	if len(r.Sources) > 32 {
		return errors.New("at most 32 sources may be selected")
	}
	return nil
}

func NormalizeInfoHash(s string) (string, error) {
	if len(s) == 40 {
		b, e := hex.DecodeString(s)
		if e == nil {
			return hex.EncodeToString(b), nil
		}
	}
	if len(s) == 32 {
		b, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(s))
		if e == nil && len(b) == 20 {
			return hex.EncodeToString(b), nil
		}
	}
	return "", errors.New("expected a BitTorrent v1 info hash")
}

func validHTTP(s string) bool {
	u, e := url.Parse(s)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && strings.IndexFunc(s, unicode.IsControl) < 0
}

func NormalizeResult(r Result) (Result, error) {
	if len(r.Link) > 64<<10 || strings.IndexFunc(r.Link, unicode.IsControl) >= 0 {
		return r, errors.New("invalid result link")
	}
	var key string
	if strings.HasPrefix(r.Link, "magnet:") {
		u, e := url.Parse(r.Link)
		if e != nil {
			return r, e
		}
		for _, xt := range u.Query()["xt"] {
			if strings.HasPrefix(xt, "urn:btih:") {
				r.InfoHash, e = NormalizeInfoHash(strings.TrimPrefix(xt, "urn:btih:"))
				if e == nil {
					break
				}
			}
		}
		if r.InfoHash == "" {
			return r, errors.New("magnet result has no supported info hash")
		}
		// Parse the chosen hash again so a malformed magnet cannot borrow a supplied hash.
		valid := false
		for _, xt := range u.Query()["xt"] {
			if strings.HasPrefix(xt, "urn:btih:") {
				h, e := NormalizeInfoHash(strings.TrimPrefix(xt, "urn:btih:"))
				if e == nil && h == r.InfoHash {
					valid = true
					break
				}
			}
		}
		if !valid {
			return r, errors.New("invalid magnet result")
		}
		r.Kind = "magnet"
		key = "bt:" + r.InfoHash
		if r.Name == "" {
			r.Name = u.Query().Get("dn")
		}
	} else if strings.HasPrefix(r.Link, "ed2k://") {
		p := strings.Split(r.Link, "|")
		if len(p) < 6 || p[0] != "ed2k://" || p[1] != "file" || p[len(p)-1] != "/" {
			return r, errors.New("invalid ed2k result")
		}
		h, e := hex.DecodeString(p[4])
		if e != nil || len(h) != 16 {
			return r, errors.New("invalid ed2k hash")
		}
		var size int64
		if _, e = fmt.Sscan(p[3], &size); e != nil || size <= 0 || fmt.Sprint(size) != p[3] {
			return r, errors.New("invalid ed2k size")
		}
		r.Size = size
		r.Kind = "ed2k"
		key = "ed2k:" + hex.EncodeToString(h) + ":" + p[3]
		if r.Name == "" {
			r.Name, _ = url.PathUnescape(p[2])
		}
	} else if validHTTP(r.Link) {
		r.Kind = "torrent"
		r.TorrentURL = r.Link
		key = "url:" + r.Link
		if r.InfoHash != "" {
			h, e := NormalizeInfoHash(r.InfoHash)
			if e != nil {
				return r, e
			}
			r.InfoHash = h
			key = "bt:" + h
		}
	} else {
		return r, errors.New("result must contain an HTTP(S) torrent, magnet, or ed2k link")
	}
	if r.TorrentURL != "" && !validHTTP(r.TorrentURL) {
		r.TorrentURL = ""
	}
	if r.PageURL != "" && !validHTTP(r.PageURL) {
		r.PageURL = ""
	}
	r.Name = strings.TrimSpace(strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.In(c, unicode.Cf) {
			return -1
		}
		return c
	}, r.Name))
	if len(r.Name) > 4096 {
		r.Name = string([]rune(r.Name)[:min(len([]rune(r.Name)), 1024)])
	}
	if r.Name == "" {
		r.Name = key
	}
	r.Size = max(0, r.Size)
	r.Seeds = max(0, r.Seeds)
	r.Peers = max(0, r.Peers)
	sum := sha256.Sum256([]byte(key))
	r.ID = hex.EncodeToString(sum[:6])
	return r, nil
}

func matchResult(r Result, kind string) bool {
	switch kind {
	case "ed2k":
		return r.Kind == "ed2k"
	case "bt":
		return r.Kind != "ed2k"
	case "magnet":
		return r.Kind == "magnet"
	case "torrent":
		return r.Kind == "torrent" || r.TorrentURL != ""
	}
	return true
}
func sortedResults(all map[string]Result, kind string) []Result {
	out := make([]Result, 0, len(all))
	for _, r := range all {
		if !matchResult(r, kind) {
			continue
		}
		r.Sources = append([]string(nil), r.Sources...)
		sort.Strings(r.Sources)
		if kind == "torrent" && r.TorrentURL != "" {
			r.Link = r.TorrentURL
			r.Kind = "torrent"
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seeds != out[j].Seeds {
			return out[i].Seeds > out[j].Seeds
		}
		if out[i].Peers != out[j].Peers {
			return out[i].Peers > out[j].Peers
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}
