package search

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultSourcesAndNativeProvider(t *testing.T) {
	sources := DefaultSources()
	if len(sources) != 7 {
		t.Fatalf("default source count = %d", len(sources))
	}
	var foundNative, foundArchive bool
	for _, source := range sources {
		switch source.ID {
		case "ed2k":
			foundNative = source.Type == "emule" && source.Enabled
		case "archive":
			foundArchive = !source.Enabled
		}
	}
	if !foundNative || !foundArchive {
		t.Fatalf("native/archive defaults missing: %+v", sources)
	}
	if got := WebProviders(sources, nil); len(got) != 5 {
		t.Fatalf("web providers = %d", len(got))
	}
	mutated := DefaultSources()
	mutated[0].Name = "changed"
	if DefaultSources()[0].Name == "changed" {
		t.Fatal("DefaultSources returned shared entries")
	}
}

func TestSourcesRoundTripIsPrivateAndAtomic(t *testing.T) {
	dir := t.TempDir()
	secret := "top-secret-api-key"
	sources := []SourceConfig{{
		ID: "private", Name: "Private", Type: "torznab",
		URL: "https://indexer.invalid/api?apikey=embedded&cat=2000&q={query}", APIKey: secret, Enabled: true,
	}}
	if err := SaveSources(dir, sources); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, searchSourcesFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("source mode = %o", info.Mode().Perm())
	}
	loaded, err := LoadSources(dir)
	if err != nil || len(loaded) != 1 || loaded[0].APIKey != secret {
		t.Fatalf("round trip = %+v, %v", loaded, err)
	}
	public := PublicSources(loaded)
	if len(public) != 1 || strings.Contains(public[0].URL, secret) || strings.Contains(public[0].URL, "embedded") {
		t.Fatalf("public source leaked credentials: %+v", public)
	}
	var raw []map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &raw); err != nil || raw[0]["api_key"] != secret {
		t.Fatalf("stored API key missing: %s", b)
	}
}

func TestLoadSourcesInitializesDefaults(t *testing.T) {
	dir := t.TempDir()
	sources, err := LoadSources(dir)
	if err != nil || len(sources) != len(DefaultSources()) {
		t.Fatalf("initial load = %d, %v", len(sources), err)
	}
	if _, err := os.Stat(filepath.Join(dir, searchSourcesFile)); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSourcesLimitsAndNoSecretInErrors(t *testing.T) {
	bad := SourceConfig{ID: "bad", Name: "Bad", Type: "rss", URL: "https://example.invalid?q={query}", APIKey: "private" + "-secret\n"}
	if err := ValidateSourceConfig(bad); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("invalid key error = %v", err)
	}
	tooMany := make([]SourceConfig, maxSources+1)
	for i := range tooMany {
		tooMany[i] = SourceConfig{ID: "s" + string(rune('a'+i%26)) + string(rune('0'+i/26)), Name: "source", Type: "emule"}
	}
	if err := SaveSources(t.TempDir(), tooMany); err == nil {
		t.Fatal("accepted more than 32 sources")
	}
}
