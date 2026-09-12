package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/engine"
)

type Job struct {
	ID         string    `json:"id"`
	Engine     string    `json:"engine"`
	Source     string    `json:"source"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	MetadataID string    `json:"metadata_id,omitempty"`
	engine.Item
}

// metadataRootID identifies the originally submitted aria2 task. aria2
// persists this ID when a torrent or magnet creates a new child on restart.
func (j Job) metadataRootID() string {
	if j.MetadataID != "" {
		return j.MetadataID
	}
	sum := sha256.Sum256([]byte(j.ID + j.Source))
	return hex.EncodeToString(sum[:8])
}

type State struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}
type Store struct {
	mu          sync.Mutex
	path        string
	state       State
	backends    map[string]engine.Backend
	health      map[string]string
	proxySource func(string, string) (string, error)
}

func NewStore(dir string, backends map[string]engine.Backend) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "jobs.json"), state: State{Version: 1, Jobs: []Job{}}, backends: backends, health: map[string]string{}}
	b, err := os.ReadFile(s.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(b, &s.state); err != nil {
			return nil, fmt.Errorf("invalid job database: %w", err)
		}
		if s.state.Version != 1 {
			return nil, errors.New("unsupported job database version")
		}
	}
	return s, nil
}

var edHash = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)

func ValidateSource(source string) (string, error) {
	if len(source) == 0 || len(source) > 64<<10 || strings.ContainsAny(source, "\x00\r\n") {
		return "", errors.New("invalid download source")
	}
	if strings.HasPrefix(source, "ed2k://") {
		p := strings.Split(source, "|")
		if len(p) < 6 || p[0] != "ed2k://" || p[1] != "file" || p[2] == "" || !edHash.MatchString(p[4]) || p[len(p)-1] != "/" {
			return "", errors.New("expected ed2k://|file|name|size|32-character-hash|/")
		}
		n, err := strconv.ParseUint(p[3], 10, 63)
		if err != nil || n == 0 {
			return "", errors.New("invalid ed2k file size")
		}
		return "amule", nil
	}
	u, err := url.Parse(source)
	if err != nil {
		return "", err
	}
	if u.Scheme == "magnet" {
		for _, xt := range u.Query()["xt"] {
			if strings.HasPrefix(xt, "urn:btih:") {
				hash := strings.TrimPrefix(xt, "urn:btih:")
				if len(hash) == 40 {
					if _, err = hex.DecodeString(hash); err == nil {
						return "aria2", nil
					}
				}
				if len(hash) == 32 {
					valid := true
					for _, r := range strings.ToUpper(hash) {
						if !(r >= 'A' && r <= 'Z' || r >= '2' && r <= '7') {
							valid = false
						}
					}
					if valid {
						return "aria2", nil
					}
				}
			}
		}
		return "", errors.New("magnet requires a valid v1 BitTorrent info hash (btih)")
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return "aria2", nil
	}
	if u.Scheme == "" && strings.HasSuffix(strings.ToLower(source), ".torrent") {
		if !filepath.IsAbs(source) {
			return "", errors.New("torrent path must be absolute")
		}
		st, err := os.Stat(source)
		if err != nil {
			return "", err
		}
		if !st.Mode().IsRegular() || st.Size() > 16<<20 {
			return "", errors.New("torrent must be a regular file at most 16 MiB")
		}
		return "aria2", nil
	}
	return "", errors.New("supported sources: ed2k file link, magnet, .torrent file, HTTP(S) URL")
}
func (s *Store) save() error { return writeJSON(s.path, s.state) }
func (s *Store) Add(ctx context.Context, source string) (Job, error) {
	kind, err := ValidateSource(source)
	if err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.state.Jobs {
		if job.Source == source && job.Status != "removed" {
			if job.Status == "error" || job.Status == "unknown" || job.Status == "adding" {
				return job, fmt.Errorf("task %s already exists in %s state; inspect it and remove before resubmitting", job.ID, job.Status)
			}
			return job, nil
		}
	}
	b := make([]byte, 6)
	if _, err = rand.Read(b); err != nil {
		return Job{}, err
	}
	j := Job{ID: hex.EncodeToString(b), Engine: kind, Source: source, Created: time.Now().UTC(), Updated: time.Now().UTC(), Item: engine.Item{Name: source, Status: "adding"}}
	if kind == "amule" {
		parts := strings.Split(source, "|")
		j.Item.ID = strings.ToLower(parts[4])
		j.Name, _ = url.PathUnescape(parts[2])
		j.Total, _ = strconv.ParseInt(parts[3], 10, 64)
	} else {
		sum := sha256.Sum256([]byte(j.ID + source))
		j.Item.ID = hex.EncodeToString(sum[:8])
	}
	s.state.Jobs = append(s.state.Jobs, j)
	idx := len(s.state.Jobs) - 1
	if err = s.save(); err != nil {
		s.state.Jobs = s.state.Jobs[:idx]
		return Job{}, err
	}
	var id string
	engineSource := source
	if kind == "aria2" && s.proxySource != nil {
		engineSource, err = s.proxySource(source, j.Item.ID)
	}
	if err != nil {
		// Preserve the failed operation below without exposing cookie data.
	} else if backend, ok := s.backends[kind].(interface {
		AddWithID(context.Context, string, string) (string, error)
	}); ok {
		id, err = backend.AddWithID(ctx, engineSource, j.Item.ID)
	} else {
		id, err = s.backends[kind].Add(ctx, engineSource)
	}
	if err != nil {
		j.Status = "error"
		j.Error = err.Error()
	} else {
		j.Item.ID = id
		j.Status = "waiting"
	}
	s.state.Jobs[idx] = j
	if saveErr := s.save(); saveErr != nil {
		return j, fmt.Errorf("engine request processed but persistence failed: %w", saveErr)
	}
	if err == nil {
		if persistent, ok := s.backends[kind].(interface{ SaveSession(context.Context) error }); ok {
			if saveErr := persistent.SaveSession(ctx); saveErr != nil {
				return j, fmt.Errorf("task %s accepted, but engine session flush failed: %w", j.ID, saveErr)
			}
		}
	}
	return j, err
}
func (s *Store) Snapshot() ([]Job, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := append([]Job{}, s.state.Jobs...)
	health := map[string]string{}
	for k, v := range s.health {
		health[k] = v
	}
	return jobs, health
}
func (s *Store) Refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	type finishedResult struct {
		backend engine.Backend
		id      string
		name    string
	}
	var finished []finishedResult
	for name, b := range s.backends {
		items, err := b.List(ctx)
		if err != nil {
			s.health[name] = err.Error()
			continue
		}
		s.health[name] = "ok"
		byID := map[string]engine.Item{}
		for _, item := range items {
			byID[item.ID] = item
		}
		for i := range s.state.Jobs {
			j := &s.state.Jobs[i]
			if j.Engine != name {
				continue
			}
			item, found := byID[j.Item.ID]
			if j.MetadataID != "" && j.MetadataID != j.Item.ID {
				if parent, ok := byID[j.MetadataID]; ok && (parent.Status == "metadata" || parent.Status == "complete" || parent.Status == "removed" || parent.Status == "error") {
					finished = append(finished, finishedResult{b, parent.ID, name})
				}
			}
			if j.Status == "removed" {
				if found && (item.Status == "removed" || item.Status == "complete" || item.Status == "error") {
					finished = append(finished, finishedResult{b, item.ID, name})
				}
				continue
			}
			if !found && name == "aria2" && j.Status != "complete" && j.Status != "error" {
				rootID := j.metadataRootID()
				if parent, ok := byID[rootID]; ok {
					// A paused metadata request may not have created its child yet.
					j.MetadataID = rootID
					item, found = parent, true
				}
			}
			if found {
				if item.Status == "metadata" {
					for _, next := range item.FollowedBy {
						if child, ok := byID[next]; ok {
							j.MetadataID = item.ID
							finished = append(finished, finishedResult{b, item.ID, name})
							item = child
							break
						}
					}
				}
				if name == "amule" {
					item.Total = j.Total
					item.Completed = int64(float64(item.Total) * item.Progress / 100)
				}
				j.Item = item
				j.Updated = time.Now().UTC()
				if item.Status == "complete" || item.Status == "removed" {
					finished = append(finished, finishedResult{b, item.ID, name})
				}
			} else if j.Status != "complete" && j.Status != "error" {
				j.Status = "unknown"
				j.Error = "engine does not report this task; inspect engine logs before retrying"
			}
		}
	}
	// Persist the terminal state and metadata handoff before dropping native
	// results. Seeding tasks retain their sessions; completed/removed tasks
	// must not be resurrected by aria2's per-torrent force-save option.
	if err := s.save(); err != nil {
		return err
	}
	cleaned := make(map[string]bool)
	for _, result := range finished {
		key := result.name + ":" + result.id
		if cleaned[key] {
			continue
		}
		cleaned[key] = true
		if cleaner, ok := result.backend.(interface {
			Forget(context.Context, string) error
		}); ok {
			if err := cleaner.Forget(ctx, result.id); err != nil {
				// The database is durable. Report native cleanup failure as
				// engine health and retry on the next refresh.
				s.health[result.name] = "clean stopped result: " + err.Error()
			}
		}
	}
	return nil
}
func (s *Store) Action(ctx context.Context, id, action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, j := range s.state.Jobs {
		if j.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return os.ErrNotExist
	}
	j := &s.state.Jobs[idx]
	b := s.backends[j.Engine]
	var err error
	switch action {
	case "pause":
		err = b.Pause(ctx, j.Item.ID)
	case "resume":
		err = b.Resume(ctx, j.Item.ID)
	case "remove":
		if j.Status != "complete" && j.Status != "error" && j.Status != "removed" {
			err = b.Remove(ctx, j.Item.ID)
		}
	default:
		return errors.New("unknown action")
	}
	if err != nil {
		return err
	}
	switch action {
	case "pause":
		j.Status = "paused"
	case "resume":
		j.Status = "waiting"
	case "remove":
		j.Status = "removed"
	}
	j.Updated = time.Now().UTC()
	if err := s.save(); err != nil {
		return err
	}
	if action == "remove" {
		if cleaner, ok := b.(interface {
			Forget(context.Context, string) error
		}); ok {
			if err := cleaner.Forget(ctx, j.Item.ID); err != nil {
				return err
			}
			if j.MetadataID != "" {
				if err := cleaner.Forget(ctx, j.MetadataID); err != nil {
					return err
				}
			}
		}
	}
	if persistent, ok := b.(interface{ SaveSession(context.Context) error }); ok {
		return persistent.SaveSession(ctx)
	}
	return nil
}
