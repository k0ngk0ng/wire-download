package search

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"
)

const maxJobs = 16
const maxResults = 1000
const jobTTL = 30 * time.Minute

type job struct {
	request  Request
	snapshot Snapshot
	results  map[string]Result
	seen     map[string]map[string]bool
	cancel   context.CancelFunc
	capped   bool
}

// Manager owns bounded, temporary search sessions. Providers never hold its
// lock while doing network I/O, and transfer management stays independent.
type Manager struct {
	mu    sync.Mutex
	ctx   context.Context
	stop  context.CancelFunc
	wg    sync.WaitGroup
	jobs  map[string]*job
	slots chan struct{}
}

func NewManager(ctx context.Context) *Manager {
	ctx, stop := context.WithCancel(ctx)
	return &Manager{ctx: ctx, stop: stop, jobs: make(map[string]*job), slots: make(chan struct{}, 6)}
}
func (m *Manager) Start(req Request, providers []Provider) (Snapshot, error) {
	if err := req.Validate(); err != nil {
		return Snapshot{}, err
	}
	if err := m.ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	selected := make([]Provider, 0, len(providers))
	known := map[string]bool{}
	for _, p := range providers {
		if !p.Info.Enabled || p.Search == nil {
			continue
		}
		if req.Type == "ed2k" && p.Info.Kind != "ed2k" || req.Type != "all" && req.Type != "ed2k" && p.Info.Kind == "ed2k" {
			continue
		}
		known[p.Info.ID] = true
		if len(req.Sources) > 0 && !slices.Contains(req.Sources, p.Info.ID) {
			continue
		}
		selected = append(selected, p)
	}
	for _, id := range req.Sources {
		if !known[id] {
			return Snapshot{}, fmt.Errorf("source %q is unavailable, disabled, or incompatible with type %s", id, req.Type)
		}
	}
	if len(selected) == 0 {
		return Snapshot{}, errors.New("no enabled search sources for this request")
	}
	if len(selected) > 32 {
		return Snapshot{}, errors.New("too many search sources")
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return Snapshot{}, err
	}
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	for id, j := range m.jobs {
		if j.snapshot.Done() && now.Sub(j.snapshot.Updated) > jobTTL {
			delete(m.jobs, id)
		}
	}
	if len(m.jobs) >= maxJobs {
		var oldest *job
		for _, j := range m.jobs {
			if j.snapshot.Done() && (oldest == nil || j.snapshot.Updated.Before(oldest.snapshot.Updated)) {
				oldest = j
			}
		}
		if oldest == nil {
			return Snapshot{}, errors.New("too many active searches; cancel an existing search")
		}
		delete(m.jobs, oldest.snapshot.ID)
	}
	ctx, cancel := context.WithTimeout(m.ctx, time.Duration(req.TimeoutSeconds)*time.Second)
	j := &job{request: req, results: map[string]Result{}, seen: map[string]map[string]bool{}, cancel: cancel,
		snapshot: Snapshot{ID: hex.EncodeToString(b), Query: req.Query, Type: req.Type, Status: "running", Started: now, Updated: now, Results: []Result{}, Sources: []SourceState{}}}
	for _, p := range selected {
		j.snapshot.Sources = append(j.snapshot.Sources, SourceState{ID: p.Info.ID, Name: p.Info.Name, Status: "queued"})
		j.seen[p.Info.ID] = map[string]bool{}
	}
	m.jobs[j.snapshot.ID] = j
	m.wg.Add(1)
	go m.run(ctx, j, selected)
	return m.copy(j), nil
}
func (m *Manager) run(ctx context.Context, j *job, providers []Provider) {
	defer m.wg.Done()
	defer j.cancel()
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			var err error
			select {
			case m.slots <- struct{}{}:
				defer func() { <-m.slots }()
				m.mu.Lock()
				j.snapshot.Sources[i].Status = "running"
				m.mu.Unlock()
				err = p.Search(ctx, j.request, func(rs []Result, progress int) { m.emit(ctx, j, i, rs, progress) })
			case <-ctx.Done():
				err = ctx.Err()
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			s := &j.snapshot.Sources[i]
			if err != nil {
				s.Status = "failed"
				s.Error = cleanMessage(err.Error())
			} else {
				s.Status = "complete"
				s.Progress = 100
			}
			j.snapshot.Updated = time.Now().UTC()
		}(i, p)
	}
	wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	failures := 0
	for _, s := range j.snapshot.Sources {
		if s.Status == "failed" {
			failures++
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		j.snapshot.Status = "cancelled"
	case failures == len(providers) && len(j.results) == 0:
		j.snapshot.Status = "failed"
	case failures > 0:
		j.snapshot.Status = "partial"
	default:
		j.snapshot.Status = "complete"
	}
	j.snapshot.Updated = time.Now().UTC()
}
func (m *Manager) emit(ctx context.Context, j *job, index int, rs []Result, progress int) {
	if ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &j.snapshot.Sources[index]
	s.Progress = max(s.Progress, min(99, max(0, progress)))
	for _, r := range rs {
		r, err := NormalizeResult(r)
		if err != nil {
			continue
		}
		r.Sources = []string{s.ID}
		if old, ok := j.results[r.ID]; ok {
			old.Seeds = max(old.Seeds, r.Seeds)
			old.Peers = max(old.Peers, r.Peers)
			if old.Size == 0 {
				old.Size = r.Size
			}
			if old.TorrentURL == "" {
				old.TorrentURL = r.TorrentURL
			}
			if old.PageURL == "" {
				old.PageURL = r.PageURL
			}
			if old.Kind == "torrent" && r.Kind == "magnet" {
				old.Kind = r.Kind
				old.Link = r.Link
			}
			if !slices.Contains(old.Sources, s.ID) {
				old.Sources = append(old.Sources, s.ID)
			}
			j.results[r.ID] = old
		} else {
			if len(j.results) >= maxResults {
				j.capped = true
				continue
			}
			j.results[r.ID] = r
		}
		if !j.seen[s.ID][r.ID] {
			j.seen[s.ID][r.ID] = true
			s.Count++
		}
	}
	j.snapshot.Updated = time.Now().UTC()
}
func (m *Manager) copy(j *job) Snapshot {
	s := j.snapshot
	s.Sources = append([]SourceState(nil), s.Sources...)
	s.Results = sortedResults(j.results, j.request.Type)
	s.Total = len(s.Results)
	s.Truncated = j.capped || s.Total > j.request.Limit
	if len(s.Results) > j.request.Limit {
		s.Results = s.Results[:j.request.Limit]
	}
	return s
}
func (m *Manager) Get(id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Snapshot{}, os.ErrNotExist
	}
	return m.copy(j), nil
}
func (m *Manager) Cancel(id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Snapshot{}, os.ErrNotExist
	}
	if !j.snapshot.Done() {
		j.cancel()
	}
	return m.copy(j), nil
}
func (m *Manager) Result(id, resultID string) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Result{}, os.ErrNotExist
	}
	r, ok := j.results[resultID]
	if !ok || !matchResult(r, j.request.Type) {
		return Result{}, os.ErrNotExist
	}
	r.Sources = append([]string(nil), r.Sources...)
	if j.request.Type == "torrent" && r.TorrentURL != "" {
		r.Link = r.TorrentURL
		r.Kind = "torrent"
	}
	return r, nil
}
func cleanMessage(s string) string {
	r := []rune(s)
	if len(r) > 1000 {
		r = r[:1000]
	}
	for i, c := range r {
		if c < 32 || c == 127 {
			r[i] = ' '
		}
	}
	return string(r)
}

// Close cancels sessions and waits for providers to release their resources.
func (m *Manager) Close() {
	m.mu.Lock()
	m.stop()
	m.mu.Unlock()
	m.wg.Wait()
}
