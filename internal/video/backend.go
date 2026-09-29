package video

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/auth"
	"github.com/k0ngk0ng/wire-download/internal/engine"
)

type Options struct {
	Binary, FFmpeg, Deno, StateDir, Downloads, Limit string
	Max                                              int
}
type task struct {
	spec   Spec
	item   engine.Item
	cancel context.CancelFunc
	done   chan struct{}
}
type Backend struct {
	ctx       context.Context
	cancel    context.CancelFunc
	resolving chan struct{}
	mu        sync.Mutex
	opts      Options
	tasks     map[string]*task
	closed    bool
	slots     chan struct{}
	wg        sync.WaitGroup
}

func New(opts Options) *Backend {
	if opts.Max < 1 {
		opts.Max = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Backend{ctx: ctx, cancel: cancel, resolving: make(chan struct{}, 2), opts: opts, tasks: map[string]*task{}, slots: make(chan struct{}, opts.Max)}
}

func (b *Backend) Check() error {
	for _, name := range []string{b.opts.Binary, b.opts.FFmpeg, b.opts.Deno} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("video engine missing: %s; install the complete wire-download distribution", filepath.Base(name))
		}
	}
	return nil
}

// Each invocation has its own cookie export. Neither process arguments nor
// persisted task metadata contain cookie values or expiring media URLs.
func (b *Backend) command(ctx context.Context, source string, args ...string) (*exec.Cmd, func(), error) {
	if err := b.Check(); err != nil {
		return nil, nil, err
	}
	tmp := filepath.Join(b.opts.StateDir, "video", "tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		return nil, nil, err
	}
	base := []string{"--ignore-config", "--no-cache-dir", "--no-colors", "--socket-timeout", "20", "--retries", "3", "--fragment-retries", "3", "--no-remote-components", "--js-runtimes", "deno:" + b.opts.Deno, "--ffmpeg-location", b.opts.FFmpeg}
	cleanup := func() {}
	origins := []string{"https://www.youtube.com", "https://youtube.com", "https://m.youtube.com", "https://youtu.be"}
	if strings.Contains(source, "x.com/") {
		origins = []string{"https://x.com", "https://twitter.com", "https://www.x.com", "https://www.twitter.com", "https://mobile.twitter.com"}
	}
	for _, origin := range origins {
		session, err := auth.Load(b.opts.StateDir, origin)
		if err != nil {
			return nil, nil, err
		}
		if session == nil {
			continue
		}
		f, err := os.CreateTemp(tmp, "cookies-*")
		if err != nil {
			return nil, nil, err
		}
		cleanup = func() { _ = os.Remove(f.Name()) }
		var data strings.Builder
		data.WriteString("# Netscape HTTP Cookie File\n")
		for _, c := range session.Cookies {
			domain := c.Domain
			// X moved its API to x.com. Export only this site's corresponding domain.
			if strings.Contains(source, "x.com/") {
				domain = strings.Replace(domain, "twitter.com", "x.com", 1)
			}
			flag, secure := "FALSE", "FALSE"
			if strings.HasPrefix(domain, ".") {
				flag = "TRUE"
			}
			if c.Secure {
				secure = "TRUE"
			}
			path := c.Path
			if path == "" {
				path = "/"
			}
			if strings.ContainsAny(domain+path+c.Name+c.Value, "\t\r\n") {
				f.Close()
				cleanup()
				return nil, nil, errors.New("login cookie cannot be exported")
			}
			expires := int64(c.Expires)
			if expires < 0 {
				expires = 0
			}
			fmt.Fprintf(&data, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", domain, flag, path, secure, expires, c.Name, c.Value)
		}
		_, err = f.WriteString(data.String())
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		base = append(base, "--cookies", f.Name(), "--user-agent", session.UserAgent)
		break
	}
	cmd := exec.CommandContext(ctx, b.opts.Binary, append(base, args...)...)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "DENO_DIR="+filepath.Join(b.opts.StateDir, "video", "deno"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd, cleanup, nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := w.max - w.Len()
	if left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = w.Buffer.Write(p)
	}
	return n, nil
}

// yt-dlp sends progress to stderr when --print reserves stdout for results.
type progressWriter struct {
	pending []byte
	onLine  func(string)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	for _, c := range p {
		if c == '\n' || c == '\r' {
			if len(w.pending) > 0 {
				w.onLine(string(w.pending))
				w.pending = nil
			}
			continue
		}
		if len(w.pending) < 1<<20 {
			w.pending = append(w.pending, c)
		}
	}
	return n, nil
}

func failure(stderr string, err error) error {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "sign in"), strings.Contains(s, "login"), strings.Contains(s, "cookies"), strings.Contains(s, "age-restricted"):
		return errors.New("video access requires a valid login; run wirectl download login for this website, then resume or retry")
	case strings.Contains(s, "429"), strings.Contains(s, "rate-limit"), strings.Contains(s, "too many requests"):
		return errors.New("website rate limit reached; retry later")
	case strings.Contains(s, "no video"), strings.Contains(s, "no video formats"), strings.Contains(s, "does not exist"), strings.Contains(s, "unavailable"), strings.Contains(s, "private video"), strings.Contains(s, "removed"):
		return errors.New("video is unavailable, private, deleted, or this post has no downloadable video")
	default:
		return fmt.Errorf("video extraction/download failed (%v); check network access, website login, or update the video engine", err)
	}
}

type info struct {
	ID         string            `json:"id"`
	Title      string            `json:"title"`
	Extractor  string            `json:"extractor_key"`
	Entries    []info            `json:"entries"`
	Formats    []json.RawMessage `json:"formats"`
	URL        string            `json:"url"`
	IsLive     bool              `json:"is_live"`
	LiveStatus string            `json:"live_status"`
}

func (b *Backend) Resolve(ctx context.Context, raw string) ([]Spec, error) {
	canonical, site, post, ok, err := Source(raw)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("unsupported video site")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("video engine stopped")
	}
	b.wg.Add(1)
	b.mu.Unlock()
	defer b.wg.Done()
	stop := context.AfterFunc(b.ctx, cancel)
	defer stop()
	select {
	case b.resolving <- struct{}{}:
		defer func() { <-b.resolving }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	args := []string{"--dump-single-json", "--skip-download", "--no-playlist", "--", canonical}
	// No index is present in canonical X URLs, so yt-dlp expands all media.
	cmd, cleanup, err := b.command(ctx, canonical, args...)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	out := &limitedBuffer{max: 16 << 20}
	stderr := &limitedBuffer{max: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("video resolution interrupted: %w", ctx.Err())
		}
		return nil, failure(stderr.String(), err)
	}
	if out.Len() >= out.max {
		return nil, errors.New("video metadata exceeds limit")
	}
	var root info
	if err = json.Unmarshal(out.Bytes(), &root); err != nil {
		return nil, errors.New("invalid video engine metadata")
	}
	entries := root.Entries
	if entries == nil {
		entries = []info{root}
	}
	if len(entries) > 32 {
		return nil, errors.New("too many videos in one post")
	}
	seen := map[string]bool{}
	var specs []Spec
	for _, e := range entries {
		if e.IsLive || e.LiveStatus == "is_upcoming" {
			return nil, errors.New("live and upcoming streams are not supported; use a finished video")
		}
		if len(e.Formats) == 0 && e.URL == "" {
			continue
		}
		if site == "youtube" && e.ID != post {
			return nil, errors.New("unexpected YouTube video identity")
		}
		if site == "x" && e.Extractor != "" && e.Extractor != "Twitter" {
			return nil, errors.New("post links to an external video; submit that video's URL directly")
		}
		spec := Spec{URL: canonical, Site: site, PostID: post, MediaID: e.ID, Index: len(specs) + 1, Title: e.Title}
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		if seen[spec.Key()] {
			continue
		}
		seen[spec.Key()] = true
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return nil, errors.New("no downloadable videos in this post")
	}
	return specs, nil
}

// Restore is also used when committing a new job. State is owned by jobs.json;
// a restart re-extracts URLs and lets yt-dlp continue its partial files.
func (b *Backend) Restore(id string, spec Spec, item engine.Item) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("video engine stopped")
	}
	if _, ok := b.tasks[id]; ok {
		return nil
	}
	item.ID = id
	if item.Name == "" {
		item.Name = spec.Filename()
	}
	t := &task{spec: spec, item: item}
	b.tasks[id] = t
	switch item.Status {
	case "paused", "complete", "error", "removed":
	default:
		b.start(t)
	}
	return nil
}
func (b *Backend) Add(context.Context, string) (string, error) {
	return "", errors.New("video tasks require resolved media metadata")
}
func (b *Backend) start(t *task) {
	ctx, cancel := context.WithCancel(b.ctx)
	t.cancel = cancel
	t.done = make(chan struct{})
	t.item.Status = "waiting"
	t.item.Error = ""
	t.item.DownloadRate = 0
	b.wg.Add(1)
	go b.run(ctx, t)
}
func (b *Backend) run(ctx context.Context, t *task) {
	defer b.wg.Done()
	defer close(t.done)
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-ctx.Done():
		return
	}
	b.mu.Lock()
	if ctx.Err() != nil {
		b.mu.Unlock()
		return
	}
	t.item.Status = "resolving"
	spec := t.spec
	b.mu.Unlock()
	err := b.download(ctx, t, spec)
	b.mu.Lock()
	defer b.mu.Unlock()
	t.item.DownloadRate = 0
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		t.item.Status = "error"
		t.item.Error = err.Error()
	} else {
		t.item.Status = "complete"
		t.item.Progress = 100
		t.item.Completed = t.item.Total
		t.item.Error = ""
	}
}
func (b *Backend) download(ctx context.Context, t *task, spec Spec) error {
	if err := os.MkdirAll(b.opts.Downloads, 0755); err != nil {
		return err
	}
	// ID filtering keeps an X task attached to the same media even if extraction
	// order changes. Do not persist signed URLs or load stale info JSON on resume.
	args := []string{"--newline", "--progress", "--progress-delta", "0.5", "--progress-template", "download:wire-progress:%(progress)j", "--print", "after_move:wire-file:%(filepath)j", "--no-simulate", "--continue", "--no-overwrites", "--no-playlist", "--match-filter", "id = '" + spec.MediaID + "'", "-f", "bv*+ba/b", "--merge-output-format", "mkv", "-o", filepath.Join(strings.ReplaceAll(b.opts.Downloads, "%", "%%"), spec.Filename()+".%(ext)s")}
	if b.opts.Limit != "" && b.opts.Limit != "0" {
		args = append(args, "--limit-rate", b.opts.Limit)
	}
	args = append(args, "--", spec.URL)
	cmd, cleanup, err := b.command(ctx, spec.URL, args...)
	if err != nil {
		return err
	}
	defer cleanup()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &limitedBuffer{max: 64 << 10}
	cmd.Stderr = &progressWriter{onLine: func(line string) {
		if strings.HasPrefix(line, "wire-progress:") {
			b.progress(t, []byte(strings.TrimPrefix(line, "wire-progress:")))
		} else {
			_, _ = stderr.Write([]byte(line + "\n"))
		}
	}}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("start video engine: %w", err)
	}
	var output string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "wire-file:") {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "wire-file:")), &output)
		} else if strings.HasPrefix(line, "wire-progress:") {
			b.progress(t, []byte(strings.TrimPrefix(line, "wire-progress:")))
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = cmd.Cancel()
		_, _ = io.Copy(io.Discard, stdout)
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scanErr != nil {
		return scanErr
	}
	if err != nil {
		return failure(stderr.String(), err)
	}
	if output == "" {
		return errors.New("video engine did not produce the selected media file")
	}
	rel, err := filepath.Rel(b.opts.Downloads, output)
	if err != nil || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") || filepath.Base(output) != rel {
		return errors.New("video engine returned an unexpected output path")
	}
	st, err := os.Stat(output)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return errors.New("video output is missing or empty")
	}
	b.mu.Lock()
	t.item.Name = filepath.Base(output)
	t.item.Total = st.Size()
	b.mu.Unlock()
	return nil
}
func (b *Backend) progress(t *task, data []byte) {
	var p struct {
		Status     string  `json:"status"`
		Downloaded float64 `json:"downloaded_bytes"`
		Total      float64 `json:"total_bytes"`
		Estimate   float64 `json:"total_bytes_estimate"`
		Speed      float64 `json:"speed"`
	}
	if json.Unmarshal(data, &p) != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.item.Status == "paused" || t.item.Status == "removed" {
		return
	}
	t.item.Status = "active"
	t.item.Completed = int64(p.Downloaded)
	t.item.Total = int64(p.Total)
	if t.item.Total == 0 {
		t.item.Total = int64(p.Estimate)
	}
	t.item.DownloadRate = int64(p.Speed)
	t.item.Progress = 0
	if t.item.Total > 0 {
		t.item.Progress = float64(t.item.Completed) * 100 / float64(t.item.Total)
		if t.item.Progress > 99.9 {
			t.item.Progress = 99.9
		}
	}
	if p.Status == "finished" {
		t.item.Status = "processing"
		t.item.DownloadRate = 0
	}
}
func (b *Backend) List(context.Context) ([]engine.Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := make([]engine.Item, 0, len(b.tasks))
	for _, t := range b.tasks {
		items = append(items, t.item)
	}
	return items, nil
}
func (b *Backend) stop(id, status string) error {
	b.mu.Lock()
	t, ok := b.tasks[id]
	if !ok {
		b.mu.Unlock()
		return os.ErrNotExist
	}
	if status == "paused" && (t.item.Status == "complete" || t.item.Status == "removed") {
		b.mu.Unlock()
		return errors.New("completed or removed video cannot be paused")
	}
	t.item.Status = status
	t.item.DownloadRate = 0
	cancel, done := t.cancel, t.done
	if cancel != nil {
		cancel()
	}
	b.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}
func (b *Backend) Pause(_ context.Context, id string) error  { return b.stop(id, "paused") }
func (b *Backend) Remove(_ context.Context, id string) error { return b.stop(id, "removed") }
func (b *Backend) Resume(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tasks[id]
	if !ok {
		return os.ErrNotExist
	}
	if b.closed {
		return errors.New("video engine stopped")
	}
	if t.item.Status == "complete" || t.item.Status == "removed" {
		return errors.New("completed or removed video cannot be resumed")
	}
	if t.done != nil {
		select {
		case <-t.done:
		default:
			return nil
		}
	}
	b.start(t)
	return nil
}
func (b *Backend) Close() error {
	b.cancel()
	b.mu.Lock()
	b.closed = true
	for _, t := range b.tasks {
		if t.cancel != nil {
			t.cancel()
		}
	}
	b.mu.Unlock()
	b.wg.Wait()
	return nil
}
