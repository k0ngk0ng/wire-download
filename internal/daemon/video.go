package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/engine"
	"github.com/k0ngk0ng/wire-download/internal/video"
)

type Submission struct {
	Jobs   []Job    `json:"jobs"`
	Errors []string `json:"errors,omitempty"`
}

func (s *Store) Submit(ctx context.Context, source string) (Submission, error) {
	if _, err := ValidateSource(source); err != nil {
		return Submission{}, err
	}
	_, _, _, handled, err := video.Source(source)
	if err != nil {
		return Submission{}, err
	}
	if !handled {
		j, err := s.Add(ctx, source)
		if err != nil {
			return Submission{}, err
		}
		return Submission{Jobs: []Job{j}}, nil
	}
	backend, ok := s.backends["yt-dlp"].(*video.Backend)
	if !ok {
		return Submission{}, errors.New("video engine unavailable; update and restart the daemon")
	}
	// Network resolution must not hold the store lock: list/pause/remove continue
	// to work while the website is responding.
	specs, err := backend.Resolve(ctx, source)
	if err != nil {
		return Submission{}, err
	}
	if err = ctx.Err(); err != nil {
		return Submission{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := Submission{Jobs: []Job{}}
	for _, spec := range specs {
		existing := -1
		for i, j := range s.state.Jobs {
			if j.Video != nil && j.Video.Key() == spec.Key() && j.Status != "removed" {
				existing = i
				break
			}
		}
		if existing >= 0 {
			j := s.state.Jobs[existing]
			result.Jobs = append(result.Jobs, j)
			if j.Status == "error" {
				result.Errors = append(result.Errors, fmt.Sprintf("video task %s failed; use resume to retry", j.ID))
			}
			continue
		}
		var token [6]byte
		if _, err = rand.Read(token[:]); err != nil {
			result.Errors = append(result.Errors, "create video identity: "+err.Error())
			break
		}
		id := hex.EncodeToString(token[:])
		now := time.Now().UTC()
		j := Job{ID: id, Engine: "yt-dlp", Source: spec.URL, Video: &spec, Created: now, Updated: now, Item: engine.Item{ID: id, Name: spec.Filename(), Status: "waiting"}}
		s.state.Jobs = append(s.state.Jobs, j)
		if err = s.save(); err != nil {
			s.state.Jobs = s.state.Jobs[:len(s.state.Jobs)-1]
			result.Errors = append(result.Errors, "persist video task: "+err.Error())
			break
		}
		if err = backend.Restore(id, spec, j.Item); err != nil {
			j.Status = "error"
			j.Error = err.Error()
			s.state.Jobs[len(s.state.Jobs)-1] = j
			result.Errors = append(result.Errors, err.Error())
			if err = s.save(); err != nil {
				result.Errors = append(result.Errors, "persist failed task: "+err.Error())
			}
		}
		result.Jobs = append(result.Jobs, j)
	}
	return result, nil
}
