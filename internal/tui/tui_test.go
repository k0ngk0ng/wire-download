package tui

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/k0ngk0ng/wire-download/internal/search"
)

func TestProgressBarBoundaries(t *testing.T) {
	for percent, want := range map[float64]int{0: 0, 50: 5, 100: 10, -1: 0, 101: 10} {
		bar := progressBar(percent)
		if strings.Count(bar, "█") != want || strings.Count(bar, "█")+strings.Count(bar, "░") != 10 {
			t.Fatal(percent, bar)
		}
	}
	if strings.Contains(progressBar(math.NaN()), "NaN") {
		t.Fatal("non-finite progress rendered")
	}
}
func TestTerminalTextCannotInjectControls(t *testing.T) {
	s := Clean("name\x1b]52;c;secret\a\n\r\u202e")
	if strings.ContainsAny(s, "\x1b\a\n\r\u202e") {
		t.Fatal(s)
	}
	if got := cell("下载文件", 5); got != "下载 " {
		t.Fatal(got)
	}
}

func TestSearchSelectionFollowsStableResultID(t *testing.T) {
	results := []search.Result{{ID: "first", Name: "first"}, {ID: "second", Name: "second"}}
	if index, selected := searchSelection(results, "second"); index != 1 || selected != "second" {
		t.Fatalf("selection=%d/%q", index, selected)
	}
	// A later poll can sort a higher-seed result ahead of the current row. The
	// selected ID must continue to identify the same result.
	reordered := []search.Result{results[1], results[0]}
	if index, selected := searchSelection(reordered, "second"); index != 0 || selected != "second" {
		t.Fatalf("reordered selection=%d/%q", index, selected)
	}
	if index, selected := searchSelection(reordered, "missing"); index != -1 || selected != "missing" {
		t.Fatalf("missing selection=%d/%q", index, selected)
	}
}

func TestSearchLayoutKeepsResultsWhenManySourcesAreConfigured(t *testing.T) {
	if sourceRows, more, resultRows := searchLayout(24, 32); sourceRows != 8 || !more || resultRows < 1 {
		t.Fatalf("layout at 24 rows=%d more=%t results=%d", sourceRows, more, resultRows)
	}
	if sourceRows, more, resultRows := searchLayout(12, 32); sourceRows != 0 || !more || resultRows < 1 {
		t.Fatalf("compact layout rows=%d more=%t results=%d", sourceRows, more, resultRows)
	}
}

func TestSearchTableContainsProgressAndStableIDs(t *testing.T) {
	snapshot := search.Snapshot{
		ID: "search-1", Query: "ubuntu", Status: "partial", Total: 2, Truncated: true,
		Sources: []search.SourceState{{ID: "source", Name: "Feed", Status: "failed", Progress: 37, Count: 1, Error: "bad\nsource"}},
		Results: []search.Result{{ID: "result-1", Kind: "magnet", Name: "Ubuntu\x1b[31m", Seeds: 4, Peers: 2, Size: 2048}},
	}
	var out bytes.Buffer
	if err := SearchTable(&out, snapshot); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"search-1", "Feed", "37%", "result-1", "Ubuntu", "showing 1 of 2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("table missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Fatal("search table emitted an escape sequence")
	}
}
