package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/k0ngk0ng/wire-download/internal/config"
)

// UpdateTrackers updates the tracker list while holding the same state lock
// used by the daemon.  The callback receives a copy of the configured list;
// returning an error leaves config.json untouched.
func UpdateTrackers(dir string, update func([]string) ([]string, error)) ([]string, error) {
	if update == nil {
		return nil, fmt.Errorf("tracker update callback is nil")
	}
	lock, err := acquire(dir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()

	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c config.Config
	if err = json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	current := append([]string(nil), c.Trackers...)
	updated, err := update(current)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		updated = []string{}
	}
	for _, tracker := range updated {
		if err := config.ValidateTracker(tracker); err != nil {
			return nil, err
		}
	}
	c.Trackers = updated
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	if err = atomicWrite(filepath.Join(dir, "config.json"), append(data, '\n')); err != nil {
		return nil, err
	}
	return append([]string(nil), updated...), nil
}
