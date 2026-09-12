package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/client"
	"github.com/k0ngk0ng/wire-download/internal/search"
	"golang.org/x/term"
)

// SearchTable writes a non-interactive view of a search snapshot. It is also
// used by the CLI after a non-TTY search completes, so it contains no cursor
// control sequences and is safe to pipe to another program.
func SearchTable(w io.Writer, snapshot search.Snapshot) error {
	if w == nil {
		return fmt.Errorf("search table writer is nil")
	}
	fmt.Fprintf(w, "SEARCH %s  %s  %s  %d results\n", Clean(snapshot.ID), Clean(snapshot.Status), Clean(snapshot.Query), snapshot.Total)
	fmt.Fprintln(w, "SOURCE                 STATUS      PROGRESS  RESULTS  ERROR")
	for _, source := range snapshot.Sources {
		errText := Clean(source.Error)
		fmt.Fprintf(w, "%-21s %-10s %3d%%      %-7d %s\n", cell(source.Name, 21), Clean(source.Status), clampPercent(source.Progress), source.Count, errText)
	}
	if len(snapshot.Sources) > 0 {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "RESULT ID      KIND     SEEDS  PEERS  SIZE       NAME")
	for _, result := range snapshot.Results {
		fmt.Fprintf(w, "%-14s %-8s %5d  %5d  %-10s %s\n", Clean(result.ID), Clean(result.Kind), result.Seeds, result.Peers, Bytes(result.Size), Clean(result.Name)+" ["+Clean(strings.Join(result.Sources, ","))+"]")
	}
	if snapshot.Truncated {
		fmt.Fprintf(w, "... showing %d of %d results (raise --limit to inspect more)\n", len(snapshot.Results), snapshot.Total)
	}
	return nil
}

func clampPercent(n int) int {
	return max(0, min(100, n))
}

// searchSelection keeps the selected result attached to its stable result ID
// when a later snapshot reorders rows by seed count.
func searchSelection(results []search.Result, selectedID string) (int, string) {
	if selectedID != "" {
		for i, result := range results {
			if result.ID == selectedID {
				return i, selectedID
			}
		}
		// Keep the old ID when the daemon's limit or filtering removes it from
		// the current window. Returning -1 prevents Enter/d from accidentally
		// downloading a different row until the user explicitly moves focus.
		return -1, selectedID
	}
	if len(results) == 0 {
		return 0, ""
	}
	return 0, results[0].ID
}

func searchLayout(height, sourceCount int) (sourceRows int, showMoreSources bool, resultRows int) {
	height = max(height, 8)
	sourceRows = min(sourceCount, min(8, max(0, height-12)))
	showMoreSources = sourceCount > sourceRows && height >= 9
	resultRows = max(0, height-9-sourceRows)
	if showMoreSources {
		resultRows = max(0, resultRows-1)
	}
	return sourceRows, showMoreSources, resultRows
}

// Search displays a live source-progress and result view. Network polling and
// downloads run in goroutines, leaving the keyboard loop responsive even when
// a source or the daemon is slow.
func Search(ctx context.Context, c *client.Client, initial search.Snapshot) error {
	if c == nil {
		return fmt.Errorf("search client is nil")
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("search needs a terminal; use --no-tui for scripts")
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer fmt.Print("\x1b[?25h\x1b[?1049l")

	viewCtx, stop := context.WithCancel(ctx)
	defer stop()
	snapshot := initial
	cancelRequested := false
	cancelActive := func() {
		if cancelRequested || snapshot.Done() {
			return
		}
		cancelRequested = true
		cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.CancelSearch(cancelCtx, initial.ID)
		cancel()
	}
	defer cancelActive()
	keys := make(chan byte, 32)
	go readSearchKeys(viewCtx, keys)
	updates := make(chan searchPollUpdate, 4)
	go pollSearch(viewCtx, c, initial.ID, updates)

	selectedID := ""
	if len(snapshot.Results) > 0 {
		selectedID = snapshot.Results[0].ID
	}
	message := ""
	pending := false
	cancelling := false
	actions := make(chan searchActionResult, 4)

	findSelection := func() int {
		var selected int
		selected, selectedID = searchSelection(snapshot.Results, selectedID)
		return selected
	}

	render := func() {
		width, height, sizeErr := term.GetSize(int(os.Stdout.Fd()))
		if sizeErr != nil {
			width, height = 110, 28
		}
		width = max(width, 30)
		height = max(height, 8)
		selected := findSelection()
		var out strings.Builder
		line := func(value string) { out.WriteString(cell(value, width-1) + "\r\n") }
		out.WriteString("\x1b[H\x1b[1;36m")
		line("WIRE DOWNLOAD   •   live search")
		out.WriteString("\x1b[0m")
		line(fmt.Sprintf("%s  %s  %s  %d results", snapshot.ID, snapshot.Status, snapshot.Query, snapshot.Total))
		line("")
		line("SOURCE                  STATUS      PROGRESS  RESULTS  ERROR")
		maxSourceRows, showMoreSources, rows := searchLayout(height, len(snapshot.Sources))
		for i := 0; i < maxSourceRows; i++ {
			source := snapshot.Sources[i]
			line(fmt.Sprintf("%-22s %-10s %3d%%      %-7d %s", cell(source.Name, 22), source.Status, clampPercent(source.Progress), source.Count, source.Error))
		}
		if omitted := len(snapshot.Sources) - maxSourceRows; omitted > 0 && showMoreSources {
			line(fmt.Sprintf("… %d more sources (use --source to focus)", omitted))
		}
		line("")
		line("RESULT ID      KIND     SEEDS  PEERS  SIZE       NAME")
		start := max(0, selected-rows+1)
		for row := 0; row < rows; row++ {
			index := start + row
			if index >= len(snapshot.Results) {
				line("")
				continue
			}
			result := snapshot.Results[index]
			prefix := "  "
			if index == selected {
				prefix = "> "
				out.WriteString("\x1b[7m")
			}
			line(prefix + fmt.Sprintf("%-14s %-8s %5d  %5d  %-10s %s", result.ID, result.Kind, result.Seeds, result.Peers, Bytes(result.Size), result.Name))
			if index == selected {
				out.WriteString("\x1b[0m")
			}
		}
		if selected >= 0 && selected < len(snapshot.Results) {
			result := snapshot.Results[selected]
			line(fmt.Sprintf("%s · %s · %s", result.ID, strings.Join(result.Sources, ","), result.Link))
		} else if selectedID != "" && len(snapshot.Results) > 0 {
			line("Selected result is outside the visible result limit; press j/k to choose")
		} else {
			line(message)
		}
		if message != "" && len(snapshot.Results) > 0 {
			line(message)
		}
		if cancelling {
			line("Cancelling search…")
		} else if pending {
			line("Downloading selected result…")
		} else {
			line("↑/↓ j/k select · Enter/d download · q cancel and quit")
		}
		out.WriteString("\x1b[J")
		fmt.Print(out.String())
	}

	render()
	for {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if update.snapshot.ID != "" {
				snapshot = update.snapshot
			}
			if update.err != nil {
				message = update.err.Error()
			}
			render()
		case action := <-actions:
			pending = false
			if action.err != nil {
				message = action.err.Error()
				cancelling = false
				cancelRequested = false
			} else {
				message = action.message
				if action.quit {
					return nil
				}
			}
			render()
		case key, ok := <-keys:
			if !ok {
				return nil
			}
			if key == 3 || key == 'q' {
				if cancelling {
					continue
				}
				if !snapshot.Done() {
					cancelling = true
					cancelRequested = true
					go cancelSearchForID(actions, c, snapshot.ID)
					render()
					continue
				}
				return nil
			}
			selected := findSelection()
			switch key {
			case 'j', 'B':
				if len(snapshot.Results) > 0 {
					selected = min(selected+1, len(snapshot.Results)-1)
					selectedID = snapshot.Results[selected].ID
				}
			case 'k', 'A':
				if len(snapshot.Results) > 0 {
					selected = max(selected-1, 0)
					selectedID = snapshot.Results[selected].ID
				}
			case '\n', '\r', 'd':
				if !pending && selected >= 0 && selected < len(snapshot.Results) {
					pending = true
					message = "Working…"
					searchID, resultID := snapshot.ID, snapshot.Results[selected].ID
					go downloadSearchResult(actions, c, searchID, resultID)
				}
			}
			render()
		}
	}
}

type searchPollUpdate struct {
	snapshot search.Snapshot
	err      error
}

func readSearchKeys(ctx context.Context, keys chan<- byte) {
	b := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(b); err != nil {
			close(keys)
			return
		}
		select {
		case keys <- b[0]:
		case <-ctx.Done():
			return
		}
	}
}

func pollSearch(ctx context.Context, c *client.Client, id string, updates chan<- searchPollUpdate) {
	defer close(updates)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
		requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		snapshot, err := c.SearchResults(requestCtx, id)
		cancel()
		select {
		case updates <- searchPollUpdate{snapshot: snapshot, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			continue
		}
		if snapshot.Done() {
			return
		}
	}
}

type searchActionResult struct {
	message string
	err     error
	quit    bool
}

func cancelSearchForID(actions chan<- searchActionResult, c *client.Client, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := c.CancelSearch(ctx, id)
	cancel()
	if err != nil {
		actions <- searchActionResult{err: err}
		return
	}
	actions <- searchActionResult{message: "Search cancelled; collected results are retained", quit: true}
}

func downloadSearchResult(actions chan<- searchActionResult, c *client.Client, searchID, resultID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	job, err := c.DownloadSearchResult(ctx, searchID, resultID)
	cancel()
	if err != nil {
		actions <- searchActionResult{err: err}
		return
	}
	actions <- searchActionResult{message: fmt.Sprintf("Queued %s  %s  %s", job.ID, job.Engine, Clean(job.Name))}
}
