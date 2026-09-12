package search

import (
	"context"
	"embed"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// These small, sanitized captures keep parser tests deterministic and
// runnable from a clean checkout. The original live-research responses are
// intentionally not part of the repository.
//
//go:embed testdata/*.response
var webTestFixtures embed.FS

func searchFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := webTestFixtures.ReadFile("testdata/" + name + ".response")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRSSFixtures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		typ         string
		wantName    string
		wantSize    int64
		wantSeeds   int64
		wantTorrent bool
	}{
		{name: "nyaa", typ: "nyaa", wantName: "[Devil-fansub] Sintel 2010[BluRay][B8D00F91]", wantSize: 548929536, wantSeeds: 0, wantTorrent: true},
		{name: "animetosho", typ: "animetosho", wantName: "[philosophy-raws][sintel][Hi444PP FLAC][4K].mkv", wantSize: 6989000000, wantTorrent: true},
		{name: "dmhy", typ: "dmhy", wantName: "[philosophy-raws][辛特尔][sintel][HI444PP FLAC][4096x1776][BT.2020+YCOGO][简繁英字幕]", wantSize: 0, wantTorrent: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results, err := parseRSS(searchFixture(t, tc.name), tc.typ, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) == 0 {
				t.Fatal("fixture produced no result")
			}
			got := results[0]
			if got.Name != tc.wantName || got.Size != tc.wantSize || (got.TorrentURL != "") != tc.wantTorrent {
				t.Fatalf("unexpected result: %+v", got)
			}
			if tc.name == "nyaa" && got.InfoHash != "d8ffc008b02c79e68510afaa851838b604d7ba70" {
				t.Fatalf("Nyaa infohash = %q", got.InfoHash)
			}
			if tc.name == "animetosho" && got.InfoHash == "" {
				t.Fatal("AnimeTosho magnet infohash missing")
			}
			if tc.name == "dmhy" && got.Size == 1 {
				t.Fatal("DMHY placeholder enclosure length was treated as size")
			}
		})
	}
}

func TestHTMLFixtures(t *testing.T) {
	btdig, err := parseBTDig(searchFixture(t, "btdig"), nil)
	if err != nil || len(btdig) < 2 {
		t.Fatalf("BTDig: %d results, %v", len(btdig), err)
	}
	if btdig[0].Size == 0 || btdig[0].InfoHash == "" {
		t.Fatalf("BTDig result lacks main size or hash: %+v", btdig[0])
	}
	linux, err := parseLinuxTracker(searchFixture(t, "linuxtracker"), nil)
	if err != nil || len(linux) < 2 {
		t.Fatalf("LinuxTracker: %d results, %v", len(linux), err)
	}
	if linux[0].Size == 0 || linux[0].Seeds == 0 || strings.Contains(linux[0].Link, "downloadcheck") || linux[0].TorrentURL != "" {
		t.Fatalf("LinuxTracker result parsed incorrectly: %+v", linux[0])
	}
}

func TestWebProviderEscapesQueryAndLimitsResponse(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss><channel><item><title>x</title><link>magnet:?xt=urn:btih:d8ffc008b02c79e68510afaa851838b604d7ba70</link></item></channel></rss>`))
	}))
	defer server.Close()
	sources := []SourceConfig{{ID: "test", Name: "test", Type: "rss", URL: server.URL + "/search?q={query}", Enabled: true}}
	providers := WebProviders(sources, server.Client())
	if len(providers) != 1 {
		t.Fatalf("providers = %d", len(providers))
	}
	var got []Result
	if err := providers[0].Search(context.Background(), Request{Query: "a&b/c"}, func(results []Result, _ int) { got = results }); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "a&b/c" || len(got) != 1 {
		t.Fatalf("query/results = %q/%d", gotQuery, len(got))
	}

	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(make([]byte, maxSearchResponse+1))
	}))
	defer large.Close()
	largeSources := []SourceConfig{{ID: "large", Name: "large", Type: "rss", URL: large.URL + "?q={query}", Enabled: true}}
	provider := WebProviders(largeSources, large.Client())[0]
	if err := provider.Search(context.Background(), Request{Query: "x"}, nil); err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatalf("large response error = %v", err)
	}
}

func TestTorznabAddsSearchParametersAndRejectsIndexerErrors(t *testing.T) {
	const hash = "d8ffc008b02c79e68510afaa851838b604d7ba70"
	var gotQuery, gotType, gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotType, gotKey = r.URL.Query().Get("q"), r.URL.Query().Get("t"), r.URL.Query().Get("apikey")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<rss><channel><item><title>x</title><link>magnet:?xt=urn:btih:` + hash + `</link></item></channel></rss>`))
	}))
	defer server.Close()
	source := SourceConfig{ID: "torznab", Name: "Torznab", Type: "torznab", URL: server.URL + "/api", APIKey: "secret", Enabled: true}
	if err := ValidateSourceConfig(source); err != nil {
		t.Fatal(err)
	}
	provider := WebProviders([]SourceConfig{source}, server.Client())[0]
	if err := provider.Search(context.Background(), Request{Query: "a&b"}, nil); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "a&b" || gotType != "search" || gotKey != "secret" {
		t.Fatalf("torznab query = %q, t = %q, key = %q", gotQuery, gotType, gotKey)
	}

	errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<rss><channel><error code="100" description="bad key"/></channel></rss>`))
	}))
	defer errServer.Close()
	errSource := SourceConfig{ID: "error", Name: "Error", Type: "torznab", URL: errServer.URL + "/api?q={query}", APIKey: "secret", Enabled: true}
	errProvider := WebProviders([]SourceConfig{errSource}, errServer.Client())[0]
	if err := errProvider.Search(context.Background(), Request{Query: "x"}, nil); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("torznab error = %v", err)
	}
}

func TestHTMLTemplateMismatchIsFailure(t *testing.T) {
	if _, err := parseBTDig([]byte(`<html><body>blocked</body></html>`), nil); err == nil {
		t.Fatal("accepted an unrelated BTDig HTML response")
	}
	if _, err := parseLinuxTracker([]byte(`<html><body>blocked</body></html>`), nil); err == nil {
		t.Fatal("accepted an unrelated LinuxTracker HTML response")
	}
	if results, err := parseBTDig([]byte(`<html><form><input name="q"/></form></html>`), nil); err != nil || len(results) != 0 {
		t.Fatalf("valid empty BTDig page = %d, %v", len(results), err)
	}
	if results, err := parseLinuxTracker([]byte(`<html><form><input name="search"/></form></html>`), nil); err != nil || len(results) != 0 {
		t.Fatalf("valid empty LinuxTracker page = %d, %v", len(results), err)
	}
}

func TestChallengeDetectionKeepsValidLinuxTrackerPage(t *testing.T) {
	if looksBlockedChallenge("text/html", searchFixture(t, "linuxtracker")) {
		t.Fatal("normal LinuxTracker footer was classified as a challenge")
	}
	if !looksBlockedChallenge("text/html", []byte(`<html><head><title>Just a Moment...</title></head></html>`)) {
		t.Fatal("browser challenge was not classified")
	}
}

func TestSourceURLPathEscaping(t *testing.T) {
	source := SourceConfig{ID: "path", Name: "path", Type: "rss", URL: "https://example.invalid/search/{query}", Enabled: true}
	expanded, err := expandSourceURL(source, "a/b c")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(expanded)
	if err != nil || u.Path != "/search/a/b c" || !strings.Contains(u.EscapedPath(), "a%2Fb%20c") {
		t.Fatalf("expanded path = %q (%v)", expanded, err)
	}
}
