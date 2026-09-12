package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func awaitSearch(t *testing.T, m *Manager, id string) Snapshot {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); {
		s, e := m.Get(id)
		if e != nil {
			t.Fatal(e)
		}
		if s.Done() {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("search did not finish")
	return Snapshot{}
}
func testProvider(id string, fn func(context.Context, Request, Emit) error) Provider {
	return Provider{Info: SourceInfo{ID: id, Name: id, Kind: "bt", Enabled: true}, Search: fn}
}
func TestManagerMergePartialAndStableDownload(t *testing.T) {
	m := NewManager(context.Background())
	hash := strings.Repeat("ab", 20)
	p1 := testProvider("one", func(_ context.Context, _ Request, e Emit) error {
		r := Result{Name: "Ubuntu", Link: "magnet:?xt=urn:btih:" + hash, Seeds: 2}
		e([]Result{r, r}, 30)
		return nil
	})
	p2 := testProvider("two", func(_ context.Context, _ Request, e Emit) error {
		e([]Result{{Name: "Ubuntu", Link: "https://example.org/a.torrent", InfoHash: hash, Seeds: 4}, {Name: "Other", Link: "https://example.org/b.torrent", Seeds: 9}}, 90)
		return errors.New("upstream failed after partial results")
	})
	s, e := m.Start(Request{Query: "ubuntu", Limit: 1}, []Provider{p1, p2})
	if e != nil {
		t.Fatal(e)
	}
	s = awaitSearch(t, m, s.ID)
	if s.Status != "partial" || s.Total != 2 || !s.Truncated || len(s.Results) != 1 {
		t.Fatalf("%+v", s)
	}
	normalized, _ := NormalizeResult(Result{Link: "magnet:?xt=urn:btih:" + hash})
	r, e := m.Result(s.ID, normalized.ID)
	if e != nil || len(r.Sources) != 2 || r.Seeds != 4 || r.Kind != "magnet" || r.TorrentURL == "" {
		t.Fatalf("%+v %v", r, e)
	}
	if s.Sources[0].Count != 1 || s.Sources[1].Count != 2 {
		t.Fatalf("%+v", s.Sources)
	}
	// Mutating a returned snapshot cannot alter retained results.
	s.Sources[0].ID = "changed"
	r.Sources[0] = "changed"
	again, _ := m.Result(s.ID, r.ID)
	if again.Sources[0] == "changed" {
		t.Fatal("shared slice")
	}
}
func TestManagerCancelAndSelection(t *testing.T) {
	m := NewManager(context.Background())
	p := testProvider("wait", func(ctx context.Context, _ Request, _ Emit) error { <-ctx.Done(); return ctx.Err() })
	if _, e := m.Start(Request{Query: "x", Sources: []string{"missing"}}, []Provider{p}); e == nil {
		t.Fatal("unknown source")
	}
	if _, e := m.Start(Request{Query: "x", Type: "ed2k"}, []Provider{p}); e == nil {
		t.Fatal("wrong kind")
	}
	s, e := m.Start(Request{Query: "x"}, []Provider{p})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.Cancel(s.ID); e != nil {
		t.Fatal(e)
	}
	if s = awaitSearch(t, m, s.ID); s.Status != "cancelled" {
		t.Fatalf("%+v", s)
	}
	if _, e = m.Result(s.ID, "nope"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
}
func TestManagerBoundsAndFailure(t *testing.T) {
	m := NewManager(context.Background())
	p := testProvider("many", func(_ context.Context, _ Request, e Emit) error {
		var rs []Result
		for i := 0; i < maxResults+5; i++ {
			rs = append(rs, Result{Name: fmt.Sprint(i), Link: fmt.Sprintf("https://example.org/%d.torrent", i)})
		}
		e(rs, 100)
		return nil
	})
	s, e := m.Start(Request{Query: "x", Limit: 200}, []Provider{p})
	if e != nil {
		t.Fatal(e)
	}
	s = awaitSearch(t, m, s.ID)
	if s.Total != 1000 || len(s.Results) != 200 || !s.Truncated {
		t.Fatalf("%+v", s)
	}
	p.Search = func(context.Context, Request, Emit) error { return errors.New("offline") }
	s, _ = m.Start(Request{Query: "x"}, []Provider{p})
	s = awaitSearch(t, m, s.ID)
	if s.Status != "failed" {
		t.Fatal(s.Status)
	}
}
func TestNormalizeResult(t *testing.T) {
	for _, link := range []string{"magnet:?xt=urn:btih:invalid", "ed2k://|file|x|12garbage|" + strings.Repeat("a", 32) + "|/", "file:///tmp/x", "javascript:alert(1)"} {
		if _, e := NormalizeResult(Result{Link: link, InfoHash: strings.Repeat("a", 40)}); e == nil {
			t.Fatal(link)
		}
	}
	a, e := NormalizeResult(Result{Link: "magnet:?xt=urn:btih:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Name: "a\x1b[31m\u202eb"})
	if e != nil {
		t.Fatal(e)
	}
	b, e := NormalizeResult(Result{Link: "https://example.org/a.torrent", InfoHash: strings.Repeat("0", 40)})
	if e != nil || a.ID != b.ID {
		t.Fatalf("%+v %+v %v", a, b, e)
	}
	if strings.ContainsAny(a.Name, "\x1b\u202e") {
		t.Fatal(a.Name)
	}
}

func TestManagerActiveLimitAndShutdown(t *testing.T) {
	m := NewManager(context.Background())
	p := testProvider("blocked", func(ctx context.Context, _ Request, _ Emit) error { <-ctx.Done(); return ctx.Err() })
	for i := 0; i < maxJobs; i++ {
		if _, err := m.Start(Request{Query: "x"}, []Provider{p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Start(Request{Query: "x"}, []Provider{p}); err == nil {
		t.Fatal("active search limit was ignored")
	}
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown left queued/active providers alive")
	}
	if _, err := m.Start(Request{Query: "x"}, []Provider{p}); !errors.Is(err, context.Canceled) {
		t.Fatal("closed manager accepted a search", err)
	}
}
