package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxSources        = 32
	maxSourceFile     = 1 << 20
	searchSourcesFile = "search-sources.json"
)

var sourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// The built-in HTTP sources intentionally use URL templates instead of
// assembling query strings in each provider.  expandSourceURL applies the
// same escaping rules to user supplied and built-in sources.
var builtInSources = []SourceConfig{
	{
		ID:      "nyaa",
		Name:    "Nyaa",
		Type:    "nyaa",
		URL:     "https://nyaa.si/?page=rss&q={query}&c=0_0&f=0",
		Enabled: true,
	},
	{
		ID:      "animetosho",
		Name:    "AnimeTosho",
		Type:    "animetosho",
		URL:     "https://feed.animetosho.org/rss2?q={query}",
		Enabled: true,
	},
	{
		ID:      "dmhy",
		Name:    "DMHY",
		Type:    "dmhy",
		URL:     "https://share.dmhy.org/topics/rss/rss.xml?keyword={query}",
		Enabled: true,
	},
	{
		ID:      "btdig",
		Name:    "BTDig",
		Type:    "btdig",
		URL:     "https://btdig.com/search?q={query}",
		Enabled: true,
	},
	{
		ID:      "linuxtracker",
		Name:    "LinuxTracker",
		Type:    "linuxtracker",
		URL:     "https://linuxtracker.org/index.php?page=torrents&search={query}&category=0&active=1",
		Enabled: true,
	},
	{
		// Archive's endpoint is documented, but it has returned intermittent
		// TLS timeouts in production probes.  Keep it available to users while
		// requiring an explicit opt-in.
		ID:      "archive",
		Name:    "Internet Archive",
		Type:    "archive",
		URL:     "https://archive.org/advancedsearch.php?q={query}%20AND%20format%3A%22Archive%20BitTorrent%22&fl%5B%5D=identifier&fl%5B%5D=title&rows=50&output=json",
		Enabled: false,
	},
	{
		// eMule is implemented by the native daemon provider.  It is kept in
		// the source list so source selection and the public API are stable.
		ID:      "ed2k",
		Name:    "eMule",
		Type:    "emule",
		Enabled: true,
	},
}

// DefaultSources returns a fresh copy of the initial source configuration.
// Callers may mutate the returned slice and its entries safely.
func DefaultSources() []SourceConfig {
	return append([]SourceConfig(nil), builtInSources...)
}

func sourceKind(typ string) string {
	if strings.EqualFold(typ, "emule") {
		return "ed2k"
	}
	return "bt"
}

func sourceType(typ string) string {
	return strings.ToLower(strings.TrimSpace(typ))
}

func validSourceURLTemplate(raw string, requireQuery bool) error {
	if len(raw) == 0 || len(raw) > 4096 {
		return errors.New("source URL must be 1–4096 bytes")
	}
	if strings.IndexFunc(raw, unicode.IsControl) >= 0 || !utf8.ValidString(raw) {
		return errors.New("source URL contains invalid characters")
	}
	queryPlaceholders := strings.Count(raw, "{query}")
	if requireQuery && queryPlaceholders != 1 {
		return errors.New("source URL must contain exactly one {query} placeholder")
	}
	if !requireQuery && queryPlaceholders > 1 {
		return errors.New("source URL may contain at most one {query} placeholder")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("source URL must be an HTTP(S) URL without userinfo")
	}
	if strings.Contains(raw, "{query}") && !strings.Contains(u.RawQuery, "{query}") && !strings.Contains(u.Path, "{query}") {
		return errors.New("source URL placeholder must be in its path or query")
	}
	withoutQuery := strings.Replace(raw, "{query}", "", 1)
	if strings.Contains(withoutQuery, "{") || strings.Contains(withoutQuery, "}") {
		// The only braces accepted by the template are the checked placeholder.
		return errors.New("source URL contains an unsupported placeholder")
	}
	return nil
}

// ValidateSourceConfig checks a source before it is persisted or exposed as
// a provider.  It deliberately never includes APIKey in an error message.
func ValidateSourceConfig(s SourceConfig) error {
	if s.ID == "" || len(s.ID) > 64 || !sourceIDPattern.MatchString(s.ID) {
		return errors.New("source ID must contain 1–64 letters, digits, '.', '_' or '-' and start with a letter or digit")
	}
	if !utf8.ValidString(s.Name) || strings.TrimSpace(s.Name) == "" || strings.IndexFunc(s.Name, unicode.IsControl) >= 0 || len(s.Name) > 256 {
		return fmt.Errorf("source %q has an invalid name", s.ID)
	}
	if s.APIKey != "" && s.Type != "torznab" {
		return fmt.Errorf("source %q: api_key is supported only for torznab", s.ID)
	}
	if sourceType(s.Type) != s.Type {
		return fmt.Errorf("source %q type must be lowercase", s.ID)
	}
	switch s.Type {
	case "emule":
		if s.URL != "" {
			return fmt.Errorf("source %q of type emule cannot have a URL", s.ID)
		}
		if s.APIKey != "" {
			return fmt.Errorf("source %q of type emule cannot have an API key", s.ID)
		}
	case "rss", "nyaa", "animetosho", "dmhy", "btdig", "linuxtracker", "archive":
		if err := validSourceURLTemplate(s.URL, true); err != nil {
			return fmt.Errorf("source %q: %w", s.ID, err)
		}
		if !utf8.ValidString(s.APIKey) || strings.IndexFunc(s.APIKey, unicode.IsControl) >= 0 || len(s.APIKey) > 4096 {
			return fmt.Errorf("source %q has an invalid API key", s.ID)
		}
	case "torznab":
		if err := validSourceURLTemplate(s.URL, false); err != nil {
			return fmt.Errorf("source %q: %w", s.ID, err)
		}
		if !utf8.ValidString(s.APIKey) || strings.IndexFunc(s.APIKey, unicode.IsControl) >= 0 || len(s.APIKey) > 4096 {
			return fmt.Errorf("source %q has an invalid API key", s.ID)
		}
	default:
		return fmt.Errorf("source %q has unsupported type %q", s.ID, s.Type)
	}
	return nil
}

func validateSources(sources []SourceConfig) error {
	if len(sources) > maxSources {
		return fmt.Errorf("at most %d search sources are allowed", maxSources)
	}
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if err := ValidateSourceConfig(source); err != nil {
			return err
		}
		if _, ok := seen[source.ID]; ok {
			return fmt.Errorf("duplicate search source ID %q", source.ID)
		}
		seen[source.ID] = struct{}{}
	}
	return nil
}

// LoadSources loads the per-daemon source configuration.  A first run is
// initialized with DefaultSources and persisted using the same atomic path as
// subsequent updates.
func LoadSources(dir string) ([]SourceConfig, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("search source directory is required")
	}
	path := filepath.Join(dir, searchSourcesFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		sources := DefaultSources()
		if err := SaveSources(dir, sources); err != nil {
			return nil, err
		}
		return sources, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat search source configuration: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("search source configuration must be a regular file")
	}
	if info.Size() > maxSourceFile {
		return nil, errors.New("search source configuration is too large")
	}
	// Existing installations may have been created before source credentials
	// were supported.  Tighten their mode on read before parsing secrets.
	if info.Mode().Perm() != 0600 {
		if err := os.Chmod(path, 0600); err != nil {
			return nil, fmt.Errorf("secure search source configuration: %w", err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open search source configuration: %w", err)
	}
	defer f.Close()
	var sources []SourceConfig
	dec := json.NewDecoder(io.LimitReader(f, maxSourceFile+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sources); err != nil {
		return nil, fmt.Errorf("decode search source configuration: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("search source configuration has trailing data")
		}
		return nil, fmt.Errorf("read search source configuration: %w", err)
	}
	if err := validateSources(sources); err != nil {
		return nil, err
	}
	return sources, nil
}

// SaveSources atomically writes the source list with owner-only permissions.
// The temporary file is created next to the destination so os.Rename remains
// atomic on the filesystems used by the daemon.
func SaveSources(dir string, sources []SourceConfig) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("search source directory is required")
	}
	if err := validateSources(sources); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create search source directory: %w", err)
	}
	if info, err := os.Stat(dir); err != nil {
		return fmt.Errorf("stat search source directory: %w", err)
	} else if !info.IsDir() {
		return errors.New("search source path is not a directory")
	}
	f, err := os.CreateTemp(dir, ".search-sources-*.tmp")
	if err != nil {
		return fmt.Errorf("create search source temporary file: %w", err)
	}
	tmp := f.Name()
	remove := true
	defer func() {
		_ = f.Close()
		if remove {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("secure search source temporary file: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sources); err != nil {
		return fmt.Errorf("encode search source configuration: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync search source configuration: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close search source configuration: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, searchSourcesFile)); err != nil {
		return fmt.Errorf("install search source configuration: %w", err)
	}
	remove = false
	// Syncing the containing directory makes the rename durable after a
	// sudden daemon or host restart.  Some unusual filesystems do not support
	// directory fsync; report that explicitly rather than claiming durability.
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open search source directory for sync: %w", err)
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil {
		return fmt.Errorf("sync search source directory: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close search source directory: %w", closeErr)
	}
	return nil
}

func sensitiveQueryKey(key string) bool {
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", "")) {
	case "apikey", "key", "token", "password", "passwd", "secret", "authorization", "auth":
		return true
	default:
		return false
	}
}

func publicSourceURL(source SourceConfig) string {
	if source.URL == "" {
		return ""
	}
	u, err := url.Parse(source.URL)
	if err != nil {
		return ""
	}
	q := u.Query()
	for key := range q {
		if sensitiveQueryKey(key) {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	result := u.String()
	for _, secret := range []string{source.APIKey, url.QueryEscape(source.APIKey)} {
		if secret != "" {
			result = strings.ReplaceAll(result, secret, "<redacted>")
		}
	}
	return result
}

// PublicSources returns source metadata suitable for the API and CLI.  The
// API key is intentionally absent from SourceInfo, and sensitive URL query
// parameters are stripped as a second defence for imported configurations.
func PublicSources(sources []SourceConfig) []SourceInfo {
	out := make([]SourceInfo, 0, len(sources))
	for _, source := range sources {
		out = append(out, SourceInfo{
			ID:      source.ID,
			Name:    source.Name,
			Type:    source.Type,
			Kind:    sourceKind(source.Type),
			URL:     publicSourceURL(source),
			Enabled: source.Enabled,
		})
	}
	return out
}
