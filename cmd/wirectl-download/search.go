package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/client"
	searchpkg "github.com/k0ngk0ng/wire-download/internal/search"
	"github.com/k0ngk0ng/wire-download/internal/tui"
	"golang.org/x/term"
)

// searchCommand implements the search session and source-management commands.
// It deliberately keeps output on stdout and progress on stderr so that the
// non-interactive form remains safe to pipe into another program.
func searchCommand(ctx context.Context, c *client.Client, args []string) error {
	if c == nil {
		return errors.New("search client is nil")
	}
	if len(args) == 0 {
		return errors.New("usage: wirectl download search [options] <keyword> [...]")
	}
	switch args[0] {
	case "results":
		return searchResultsCommand(ctx, c, args[1:])
	case "cancel":
		return searchCancelCommand(ctx, c, args[1:])
	case "download":
		return searchDownloadCommand(ctx, c, args[1:])
	case "sources":
		return searchSourcesCommand(ctx, c, args[1:])
	default:
		return runSearchCommand(ctx, c, args)
	}
}

func runSearchCommand(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeName := fs.String("type", "all", "Result type: all, ed2k, bt, magnet, or torrent")
	sourceNames := fs.String("source", "", "Comma-separated source IDs")
	limit := fs.Int("limit", 50, "Maximum results to display (1–200)")
	timeout := fs.Duration("timeout", time.Minute, "Search timeout (for example 30s or 2m)")
	ed2kMode := fs.String("ed2k-mode", "all", "eD2k mode: all, server, global, or kad")
	jsonOutput := fs.Bool("json", false, "Print the final snapshot as JSON")
	noTUI := fs.Bool("no-tui", false, "Disable the interactive terminal view")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keywords := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if keywords == "" {
		return errors.New("usage: wirectl download search [options] <keyword> [...]")
	}
	seconds, err := durationSeconds(*timeout)
	if err != nil {
		return fmt.Errorf("--timeout: %w", err)
	}
	req := searchpkg.Request{
		Query:          keywords,
		Type:           strings.TrimSpace(*typeName),
		Sources:        splitSearchSources(*sourceNames),
		Limit:          *limit,
		TimeoutSeconds: seconds,
		ED2KMode:       strings.TrimSpace(*ed2kMode),
	}
	if err := req.Validate(); err != nil {
		return err
	}
	snapshot, err := c.Search(ctx, req)
	if err != nil {
		return err
	}
	interactive := !*noTUI && !*jsonOutput && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	if interactive {
		return tui.Search(ctx, c, snapshot)
	}
	return pollSearchCommand(ctx, c, snapshot, *jsonOutput, os.Stdout, os.Stderr)
}

func durationSeconds(d time.Duration) (int, error) {
	if d <= 0 {
		return 0, errors.New("must be positive")
	}
	// The daemon stores whole seconds. Rounding up prevents a request such as
	// 5.1s from being shortened below the value the user supplied.
	seconds := d / time.Second
	if d%time.Second != 0 {
		seconds++
	}
	if int64(seconds) > int64(^uint(0)>>1) {
		return 0, errors.New("is too large")
	}
	return int(seconds), nil
}

func splitSearchSources(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

func pollSearchCommand(ctx context.Context, c *client.Client, initial searchpkg.Snapshot, jsonOutput bool, out, progress io.Writer) error {
	snapshot := initial
	defer func() {
		if snapshot.ID == "" || snapshot.Done() {
			return
		}
		// A cancelled CLI context must also stop the daemon-side providers;
		// otherwise a native search can continue until its own long timeout.
		cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.CancelSearch(cancelCtx, snapshot.ID)
		cancel()
	}()
	lastProgress := ""
	writeProgress := func(s searchpkg.Snapshot) {
		line := searchProgressLine(s)
		if line == lastProgress {
			return
		}
		lastProgress = line
		if progress != nil {
			fmt.Fprintln(progress, line)
		}
	}
	writeProgress(snapshot)
	for !snapshot.Done() {
		wait := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			wait.Stop()
			return ctx.Err()
		case <-wait.C:
		}
		requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		updated, err := c.SearchResults(requestCtx, snapshot.ID)
		cancel()
		if err != nil {
			return err
		}
		snapshot = updated
		writeProgress(snapshot)
	}
	if jsonOutput {
		if err := json.NewEncoder(out).Encode(snapshot); err != nil {
			return err
		}
	} else if err := tui.SearchTable(out, snapshot); err != nil {
		return err
	}
	if snapshot.Status == "failed" {
		return fmt.Errorf("search %s failed", snapshot.ID)
	}
	return nil
}

func searchProgressLine(s searchpkg.Snapshot) string {
	parts := make([]string, 0, len(s.Sources))
	for _, source := range s.Sources {
		part := fmt.Sprintf("%s %s %d%% (%d)", tui.Clean(source.Name), tui.Clean(source.Status), max(0, min(100, source.Progress)), max(0, source.Count))
		if source.Error != "" {
			part += ": " + tui.Clean(source.Error)
		}
		parts = append(parts, part)
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return fmt.Sprintf("search %s: %s, %d results", s.ID, s.Status, s.Total)
	}
	return fmt.Sprintf("search %s: %s, %d results | %s", s.ID, s.Status, s.Total, strings.Join(parts, "; "))
}

func searchResultsCommand(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("search results", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOutput := fs.Bool("json", false, "Print the snapshot as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: wirectl download search results [--json] <search-id>")
	}
	snapshot, err := c.SearchResults(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(snapshot)
	}
	return tui.SearchTable(os.Stdout, snapshot)
}

func searchCancelCommand(ctx context.Context, c *client.Client, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wirectl download search cancel <search-id>")
	}
	if err := c.CancelSearch(ctx, args[0]); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "cancelled", args[0])
	return nil
}

func searchDownloadCommand(ctx context.Context, c *client.Client, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: wirectl download search download <search-id> <result-id>")
	}
	job, err := c.DownloadSearchResult(ctx, args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "%s  %s  %s\n", job.ID, job.Engine, tui.Clean(job.Name))
	return nil
}

func searchSourcesCommand(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 {
		return searchSourcesListCommand(ctx, c, nil)
	}
	switch args[0] {
	case "list":
		return searchSourcesListCommand(ctx, c, args[1:])
	case "add":
		return searchSourcesAddCommand(ctx, c, args[1:])
	case "enable", "disable":
		return searchSourcesToggleCommand(ctx, c, args[0], args[1:])
	case "remove":
		return searchSourcesRemoveCommand(ctx, c, args[1:])
	default:
		return errors.New("usage: wirectl download search sources list|add|enable|disable|remove")
	}
}

func searchSourcesListCommand(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("search sources list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOutput := fs.Bool("json", false, "Print sources as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: wirectl download search sources list [--json]")
	}
	sources, err := c.SearchSources(ctx)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(sources)
	}
	fmt.Fprintln(os.Stdout, "ID                 TYPE     ENABLED  NAME")
	for _, source := range sources {
		fmt.Fprintf(os.Stdout, "%-18s %-8s %-8t %s\n", tui.Clean(source.ID), tui.Clean(source.Type), source.Enabled, tui.Clean(source.Name))
	}
	return nil
}

func searchSourcesAddCommand(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("search sources add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.String("id", "", "Stable source ID")
	name := fs.String("name", "", "Display name")
	typeName := fs.String("type", "", "Source type")
	urlValue := fs.String("url", "", "Source URL")
	apiKeyEnv := fs.String("api-key-env", "", "Environment variable containing the Torznab API key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*id) == "" || strings.TrimSpace(*name) == "" || strings.TrimSpace(*typeName) == "" || (strings.TrimSpace(*urlValue) == "" && strings.TrimSpace(*typeName) != "emule") {
		return errors.New("usage: wirectl download search sources add --id ID --name NAME --type TYPE --url URL [--api-key-env ENV]")
	}
	apiKey := ""
	if envName := strings.TrimSpace(*apiKeyEnv); envName != "" {
		var ok bool
		apiKey, ok = os.LookupEnv(envName)
		if !ok || apiKey == "" {
			return fmt.Errorf("API key environment variable %q is not set", envName)
		}
	}
	source := searchpkg.SourceConfig{ID: strings.TrimSpace(*id), Name: strings.TrimSpace(*name), Type: strings.TrimSpace(*typeName), URL: strings.TrimSpace(*urlValue), Enabled: true, APIKey: apiKey}
	if err := c.SetSearchSource(ctx, source); err != nil {
		return err
	}
	// Never print the key, the environment variable name, or the request body.
	fmt.Fprintln(os.Stdout, "added", source.ID)
	return nil
}

func searchSourcesToggleCommand(ctx context.Context, c *client.Client, operation string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wirectl download search sources %s <source-id>", operation)
	}
	enabled := operation == "enable"
	if err := c.EnableSearchSource(ctx, args[0], enabled); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "%s %s\n", operation, args[0])
	return nil
}

func searchSourcesRemoveCommand(ctx context.Context, c *client.Client, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wirectl download search sources remove <source-id>")
	}
	if err := c.RemoveSearchSource(ctx, args[0]); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "removed", args[0])
	return nil
}
