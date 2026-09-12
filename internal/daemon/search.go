package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync"

	"github.com/k0ngk0ng/wire-download/internal/config"
	"github.com/k0ngk0ng/wire-download/internal/search"
)

type SearchService struct {
	manager *search.Manager
	mu      sync.Mutex
	dir     string
	sources []search.SourceConfig
	http    *http.Client
	native  func(context.Context, search.Request, search.Emit) error
}

func NewSearchService(ctx context.Context, dir string, native func(context.Context, search.Request, search.Emit) error) (*SearchService, error) {
	sources, err := search.LoadSources(dir)
	if err != nil {
		return nil, err
	}
	client, err := search.NewHTTPClient(config.FallbackCAFile())
	if err != nil {
		return nil, err
	}
	return &SearchService{manager: search.NewManager(ctx), dir: dir, sources: sources, http: client, native: native}, nil
}
func (s *SearchService) providers() []search.Provider {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := search.WebProviders(s.sources, s.http)
	for _, info := range search.PublicSources(s.sources) {
		if info.Type == "emule" && s.native != nil {
			ps = append(ps, search.Provider{Info: info, Search: s.native})
		}
	}
	return ps
}
func (s *SearchService) routes(mux *http.ServeMux, store *Store) {
	respond := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, code int, err error) {
		if errors.Is(err, os.ErrNotExist) {
			code = 404
		}
		respond(w, code, map[string]string{"error": err.Error()})
	}
	decode := func(w http.ResponseWriter, r *http.Request, v any) error {
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		return d.Decode(v)
	}
	mux.HandleFunc("POST /v1/searches", func(w http.ResponseWriter, r *http.Request) {
		var req search.Request
		if err := decode(w, r, &req); err != nil {
			fail(w, 400, err)
			return
		}
		snap, err := s.manager.Start(req, s.providers())
		if err != nil {
			fail(w, 422, err)
			return
		}
		respond(w, 202, snap)
	})
	mux.HandleFunc("GET /v1/searches/{id}", func(w http.ResponseWriter, r *http.Request) {
		snap, err := s.manager.Get(r.PathValue("id"))
		if err != nil {
			fail(w, 422, err)
			return
		}
		respond(w, 200, snap)
	})
	mux.HandleFunc("DELETE /v1/searches/{id}", func(w http.ResponseWriter, r *http.Request) {
		snap, err := s.manager.Cancel(r.PathValue("id"))
		if err != nil {
			fail(w, 422, err)
			return
		}
		respond(w, 200, snap)
	})
	mux.HandleFunc("POST /v1/searches/{id}/results/{result}/download", func(w http.ResponseWriter, r *http.Request) {
		result, err := s.manager.Result(r.PathValue("id"), r.PathValue("result"))
		if err != nil {
			fail(w, 422, err)
			return
		}
		job, err := store.Add(r.Context(), result.Link)
		if err != nil {
			fail(w, 422, err)
			return
		}
		respond(w, 201, job)
	})
	mux.HandleFunc("GET /v1/search-sources", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		out := search.PublicSources(s.sources)
		s.mu.Unlock()
		respond(w, 200, out)
	})
	mux.HandleFunc("PUT /v1/search-sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		var source search.SourceConfig
		if err := decode(w, r, &source); err != nil {
			fail(w, 400, err)
			return
		}
		if source.ID != r.PathValue("id") {
			fail(w, 400, errors.New("source ID must match URL"))
			return
		}
		if err := search.ValidateSourceConfig(source); err != nil {
			fail(w, 400, err)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		next := append([]search.SourceConfig(nil), s.sources...)
		found := false
		for i := range next {
			if next[i].ID == source.ID {
				next[i] = source
				found = true
				break
			}
		}
		if !found {
			next = append(next, source)
		}
		if err := search.SaveSources(s.dir, next); err != nil {
			fail(w, 422, err)
			return
		}
		s.sources = next
		respond(w, 200, search.PublicSources([]search.SourceConfig{source})[0])
	})
	mux.HandleFunc("PATCH /v1/search-sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decode(w, r, &req); err != nil {
			fail(w, 400, err)
			return
		}
		if req.Enabled == nil {
			fail(w, 400, errors.New("enabled is required"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		next := append([]search.SourceConfig(nil), s.sources...)
		for i := range next {
			if next[i].ID == r.PathValue("id") {
				next[i].Enabled = *req.Enabled
				if err := search.SaveSources(s.dir, next); err != nil {
					fail(w, 422, err)
					return
				}
				s.sources = next
				respond(w, 200, search.PublicSources([]search.SourceConfig{next[i]})[0])
				return
			}
		}
		fail(w, 404, os.ErrNotExist)
	})
	mux.HandleFunc("DELETE /v1/search-sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		next := make([]search.SourceConfig, 0, len(s.sources))
		found := false
		for _, source := range s.sources {
			if source.ID == r.PathValue("id") {
				found = true
			} else {
				next = append(next, source)
			}
		}
		if !found {
			fail(w, 404, os.ErrNotExist)
			return
		}
		if err := search.SaveSources(s.dir, next); err != nil {
			fail(w, 422, err)
			return
		}
		s.sources = next
		respond(w, 200, map[string]bool{"ok": true})
	})
}
