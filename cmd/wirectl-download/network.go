package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/k0ngk0ng/wire-download/internal/config"
	"github.com/k0ngk0ng/wire-download/internal/daemon"
)

func btCommand(ctx context.Context, dir string, args []string) error {
	if len(args) == 0 || args[0] != "trackers" {
		return errors.New("usage: wirectl download bt trackers list|add|remove")
	}
	return btTrackersCommand(ctx, dir, args[1:])
}

func btTrackersCommand(_ context.Context, dir string, args []string) error {
	if len(args) == 0 {
		return btTrackersListCommand(dir, nil)
	}
	switch args[0] {
	case "list":
		return btTrackersListCommand(dir, args[1:])
	case "add", "remove":
		return btTrackersEditCommand(dir, args[0], args[1:])
	default:
		return errors.New("usage: wirectl download bt trackers list|add|remove")
	}
}

func btTrackersListCommand(dir string, args []string) error {
	fs := flag.NewFlagSet("bt trackers list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOutput := fs.Bool("json", false, "Print trackers as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: wirectl download bt trackers list [--json]")
	}
	c, err := config.Load(dir)
	if err != nil {
		return err
	}
	trackers := c.Trackers
	if trackers == nil {
		trackers = []string{}
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(trackers)
	}
	for _, tracker := range trackers {
		fmt.Fprintln(os.Stdout, tracker)
	}
	return nil
}

func btTrackersEditCommand(dir, operation string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wirectl download bt trackers %s <tracker-url> [...]", operation)
	}
	trackers := make([]string, len(args))
	for i, raw := range args {
		tracker := strings.TrimSpace(raw)
		if err := config.ValidateTracker(tracker); err != nil {
			return fmt.Errorf("invalid tracker %q: %w", raw, err)
		}
		trackers[i] = tracker
	}
	_, err := daemon.UpdateTrackers(dir, func(current []string) ([]string, error) {
		updated := append([]string(nil), current...)
		for _, tracker := range trackers {
			switch operation {
			case "add":
				if !containsString(updated, tracker) {
					updated = append(updated, tracker)
				}
			case "remove":
				found := false
				kept := updated[:0]
				for _, existing := range updated {
					if existing == tracker {
						found = true
						continue
					}
					kept = append(kept, existing)
				}
				if !found {
					return nil, fmt.Errorf("tracker %q is not configured", tracker)
				}
				updated = kept
			}
		}
		return updated, nil
	})
	if err != nil {
		return err
	}
	verb := operation + "ed"
	if operation == "remove" {
		verb = "removed"
	}
	for _, tracker := range trackers {
		fmt.Fprintln(os.Stdout, verb, tracker)
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func emuleCommand(ctx context.Context, dir string, args []string) error {
	if len(args) != 2 || args[0] != "servers" || args[1] != "update" {
		return errors.New("usage: wirectl download emule servers update")
	}
	return daemon.UpdateServers(ctx, dir)
}
