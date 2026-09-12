package tui

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/k0ngk0ng/wire-download/internal/client"
	"github.com/k0ngk0ng/wire-download/internal/daemon"
	"golang.org/x/term"
)

func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, s)
}
func Bytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB"} {
		v /= 1024
		if v < 1024 || u == "TiB" {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return ""
}

func progressBar(percent float64) string {
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		percent = 0
	}
	percent = max(0, min(100, percent))
	filled := int(percent / 10)
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", 10-filled) + fmt.Sprintf("] %5.1f%%", percent)
}
func cell(s string, width int) string {
	s = Clean(s)
	var out strings.Builder
	n := 0
	for _, r := range s {
		w := 1
		if r >= 0x1100 && (r <= 0x115f || r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7af || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f || r >= 0xff01 && r <= 0xff60 || r >= 0x1f300) {
			w = 2
		}
		if unicode.Is(unicode.Mn, r) {
			w = 0
		}
		if n+w > width {
			break
		}
		out.WriteRune(r)
		n += w
	}
	out.WriteString(strings.Repeat(" ", max(0, width-n)))
	return out.String()
}
func visible(jobs []daemon.Job) []daemon.Job {
	out := []daemon.Job{}
	for _, j := range jobs {
		if j.Status != "removed" {
			out = append(out, j)
		}
	}
	return out
}
func Table(w io.Writer, status client.Status) {
	fmt.Fprintln(w, "ID            ENGINE  STATUS      PROGRESS     DOWN/s      NAME")
	for _, j := range visible(status.Jobs) {
		fmt.Fprintf(w, "%s  %-6s  %-10s  %6.1f%%  %10s  %s\n", j.ID, j.Engine, j.Status, j.Progress, Bytes(j.DownloadRate), Clean(j.Name))
	}
	for _, name := range []string{"aria2", "amule"} {
		fmt.Fprintf(w, "%s: %s\n", name, Clean(status.Engines[name]))
	}
}
func Watch(ctx context.Context, c *client.Client) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("watch needs a terminal; use list --json for scripts")
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys := make(chan byte, 32)
	go func() {
		b := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(b); err != nil {
				return
			}
			select {
			case keys <- b[0]:
			case <-ctx.Done():
				return
			}
		}
	}()
	type result struct {
		s   client.Status
		err error
	}
	updates := make(chan result, 1)
	wake := make(chan struct{}, 1)
	go func() {
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			s, err := c.Status(ctx)
			select {
			case updates <- result{s, err}:
			case <-ctx.Done():
				return
			}
			select {
			case <-timer.C:
			case <-wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	var status client.Status
	selected := 0
	message := ""
	confirm := false
	pending := false
	actions := make(chan error, 1)
	render := func() {
		width, height, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			width, height = 100, 24
		}
		width = max(width, 20)
		height = max(height, 5)
		jobs := visible(status.Jobs)
		selected = max(0, min(selected, len(jobs)-1))
		var out strings.Builder
		line := func(s string) { out.WriteString(cell(s, width-1) + "\r\n") }
		out.WriteString("\x1b[H")
		out.WriteString("\x1b[1;36m")
		line("WIRE DOWNLOAD   •   local daemon / live transfers")
		out.WriteString("\x1b[0m")
		line(fmt.Sprintf("aria2: %s  |  aMule: %s  |  %d tasks", status.Engines["aria2"], status.Engines["amule"], len(jobs)))
		line("")
		nameWidth := min(28, max(10, width-49))
		line("  " + cell("NAME", nameWidth) + " " + cell("STATE", 10) + " " + cell("PROGRESS", 20) + " DOWN/s")
		rows := max(0, height-9)
		start := max(0, selected-rows+1)
		for row := 0; row < rows; row++ {
			idx := start + row
			if idx >= len(jobs) {
				line("")
				continue
			}
			j := jobs[idx]
			prefix := "  "
			if idx == selected {
				prefix = "> "
				out.WriteString("\x1b[7m")
			}
			line(prefix + cell(j.Name, nameWidth) + " " + cell(j.Status, 10) + " " + progressBar(j.Progress) + fmt.Sprintf(" %10s", Bytes(j.DownloadRate)))
			out.WriteString("\x1b[0m")
		}
		if len(jobs) > 0 {
			j := jobs[selected]
			line(fmt.Sprintf("%s · %s · %s / %s · ↑ %s/s", j.ID, j.Engine, Bytes(j.Completed), Bytes(j.Total), Bytes(j.UploadRate)))
			if j.Error != "" && message == "" {
				line(j.Error)
			} else {
				line(message)
			}
		} else {
			line("Add: wirectl download 'magnet:…'")
			line(message)
		}
		line("↑/↓ j/k select · p pause · r resume · d remove · q quit")
		out.WriteString("\x1b[J")
		fmt.Print(out.String())
	}
	render()
	for {
		select {
		case <-ctx.Done():
			return nil
		case res := <-updates:
			if res.err != nil {
				message = res.err.Error()
			} else {
				status = res.s
			}
			render()
		case err := <-actions:
			pending = false
			if err != nil {
				message = err.Error()
			} else {
				message = "Done"
			}
			select {
			case wake <- struct{}{}:
			default:
			}
			render()
		case key := <-keys:
			if key == 'q' || key == 3 {
				return nil
			}
			jobs := visible(status.Jobs)
			if confirm {
				confirm = false
				if key != 'y' {
					message = "Cancelled"
					render()
					continue
				}
				key = 'D'
			}
			switch key {
			case 'j', 'B':
				selected++
			case 'k', 'A':
				selected--
			case 'd':
				if len(jobs) > 0 && !pending {
					confirm = true
					message = "Remove task and unfinished eD2k data? y confirms."
				}
			case 'p', 'r', 'D':
				if len(jobs) > 0 && !pending {
					selected = max(0, min(selected, len(jobs)-1))
					action := map[byte]string{'p': "pause", 'r': "resume", 'D': "remove"}[key]
					id := jobs[selected].ID
					pending = true
					message = "Working…"
					go func() { actions <- c.Action(ctx, id, action) }()
				}
			}
			render()
		}
	}
}
