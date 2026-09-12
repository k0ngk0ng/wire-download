package search

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in public availability smoke test. Release gating uses owned fixtures;
// transient failures of third-party sites must not break reproducible builds.
func TestLiveWebSources(t *testing.T) {
	if os.Getenv("WIRECTL_TEST_SEARCH_LIVE") != "1" {
		t.Skip("set WIRECTL_TEST_SEARCH_LIVE=1 to query public indexes")
	}
	for _, p := range WebProviders(DefaultSources(), &http.Client{Timeout: 25 * time.Second}) {
		if !p.Info.Enabled {
			continue
		}
		t.Run(p.Info.ID, func(t *testing.T) {
			query := "sintel"
			if strings.Contains(strings.ToLower(p.Info.ID), "linux") || strings.Contains(strings.ToLower(p.Info.ID), "btdig") {
				query = "ubuntu"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req := Request{Query: query, Type: "bt"}
			if err := req.Validate(); err != nil {
				t.Fatal(err)
			}
			count := 0
			err := p.Search(ctx, req, func(rs []Result, _ int) {
				for _, r := range rs {
					if _, err := NormalizeResult(r); err != nil {
						t.Errorf("invalid live result: %v", err)
					} else {
						count++
					}
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s query=%q valid results=%d", p.Info.ID, query, count)
		})
	}
}
