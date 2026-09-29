package video

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/auth"
	"github.com/k0ngk0ng/wire-download/internal/engine"
)

func TestSource(t *testing.T) {
	for _, raw := range []string{"https://twitter.com/name/status/123?s=20", "https://x.com/i/web/status/123/video/2", "https://mobile.twitter.com/name/status/123"} {
		u, site, id, ok, err := Source(raw)
		if err != nil || !ok || site != "x" || id != "123" || u != "https://x.com/i/web/status/123" {
			t.Fatalf("%s: %s %v", raw, u, err)
		}
	}
	for _, raw := range []string{"https://youtu.be/BaW_jenozKc?t=3", "https://www.youtube.com/watch?v=BaW_jenozKc&list=PL123", "https://youtube.com/shorts/BaW_jenozKc"} {
		u, site, _, ok, err := Source(raw)
		if err != nil || !ok || site != "youtube" || u != "https://www.youtube.com/watch?v=BaW_jenozKc" {
			t.Fatalf("%s: %s %v", raw, u, err)
		}
	}
	for _, raw := range []string{"https://youtube.com/playlist?list=123", "https://x.com/home", "https://user:pass@x.com/a/status/1", "https://x.com:8443/a/status/1"} {
		_, _, _, ok, err := Source(raw)
		if !ok || err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"https://example.com/movie.mp4", "https://x.com.evil.test/a/status/1", "https://video.twimg.com/a.mp4"} {
		_, _, _, ok, _ := Source(raw)
		if ok {
			t.Fatalf("claimed %s", raw)
		}
	}
}

// A real subprocess exercises cancellation, stdout framing and process waits.
func TestVideoHelper(t *testing.T) {
	if os.Getenv("WIRE_VIDEO_HELPER") != "1" {
		return
	}
	args := os.Args
	value := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	if value("--dump-single-json") != "" {
		fmt.Println(os.Getenv("WIRE_VIDEO_INFO"))
		os.Exit(0)
	}
	if os.Getenv("WIRE_VIDEO_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "ERROR: Sign in to confirm your age secret-cookie-value")
		os.Exit(1)
	}
	output := strings.ReplaceAll(value("-o"), "%(ext)s", "mp4")
	if os.Getenv("WIRE_VIDEO_SLOW") == "1" {
		for i := 0; i < 100; i++ {
			_ = os.WriteFile(output+".part", []byte("partial"), 0600)
			fmt.Fprintf(os.Stderr, "wire-progress:{\"status\":\"downloading\",\"downloaded_bytes\":%d,\"total_bytes\":100,\"speed\":12}\n", i+1)
			time.Sleep(20 * time.Millisecond)
		}
	}
	if os.Getenv("WIRE_VIDEO_NO_OUTPUT") != "1" {
		_ = os.WriteFile(output, []byte("video content"), 0600)
		p, _ := json.Marshal(output)
		fmt.Printf("wire-file:%s\n", p)
	}
	os.Exit(0)
}
func helper(t *testing.T) *Backend {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "yt-dlp")
	body := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=TestVideoHelper -- \"$@\"\n"
	if err = os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WIRE_VIDEO_HELPER", "1")
	t.Setenv("WIRE_VIDEO_INFO", `{"entries":[{"id":"101","title":"one","formats":[{}]},{"id":"102","title":"two","formats":[{}]}]}`)
	b := New(Options{Binary: script, FFmpeg: script, Deno: script, StateDir: dir, Downloads: filepath.Join(dir, "downloads"), Max: 1})
	t.Cleanup(func() { b.Close() })
	return b
}
func await(t *testing.T, b *Backend, id, status string) engine.Item {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		items, _ := b.List(context.Background())
		for _, item := range items {
			if item.ID == id && item.Status == status {
				return item
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	items, _ := b.List(context.Background())
	t.Fatalf("waiting for %s %s: %+v", id, status, items)
	return engine.Item{}
}
func TestResolveMultipleAndRejectUnavailable(t *testing.T) {
	b := helper(t)
	specs, err := b.Resolve(context.Background(), "https://twitter.com/user/status/123")
	if err != nil || len(specs) != 2 {
		t.Fatalf("%+v %v", specs, err)
	}
	if specs[0].Key() == specs[1].Key() {
		t.Fatal("media identities collapsed")
	}
	t.Setenv("WIRE_VIDEO_INFO", `{"entries":[]}`)
	if _, err = b.Resolve(context.Background(), specs[0].URL); err == nil {
		t.Fatal("accepted empty post")
	}
	t.Setenv("WIRE_VIDEO_INFO", `{"id":"101","formats":[{}],"is_live":true}`)
	if _, err = b.Resolve(context.Background(), specs[0].URL); err == nil {
		t.Fatal("accepted live stream")
	}
}
func TestDownloadPauseResumeAndRestore(t *testing.T) {
	b := helper(t)
	t.Setenv("WIRE_VIDEO_SLOW", "1")
	specs, err := b.Resolve(context.Background(), "https://x.com/a/status/123")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Restore("one", specs[0], engine.Item{Status: "waiting"}); err != nil {
		t.Fatal(err)
	}
	await(t, b, "one", "active")
	if err = b.Pause(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	paused := await(t, b, "one", "paused")
	if _, err = os.Stat(filepath.Join(b.opts.Downloads, specs[0].Filename()+".mp4.part")); err != nil {
		t.Fatal("partial file was lost", err)
	}
	b.Close()
	next := New(b.opts)
	defer next.Close()
	if err = next.Restore("one", specs[0], paused); err != nil {
		t.Fatal(err)
	}
	await(t, next, "one", "paused")
	if err = next.Resume(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	item := await(t, next, "one", "complete")
	if item.Total != 13 || item.Progress != 100 {
		t.Fatalf("%+v", item)
	}
	if err = next.Pause(context.Background(), "one"); err == nil {
		t.Fatal("paused completed video")
	}
}
func TestNoOutputAndPrivateErrors(t *testing.T) {
	b := helper(t)
	specs, err := b.Resolve(context.Background(), "https://x.com/a/status/123")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WIRE_VIDEO_NO_OUTPUT", "1")
	if err = b.Restore("one", specs[0], engine.Item{}); err != nil {
		t.Fatal(err)
	}
	await(t, b, "one", "error")
	t.Setenv("WIRE_VIDEO_FAIL", "1")
	if err = b.Resume(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	item := await(t, b, "one", "error")
	if strings.Contains(item.Error, "secret-cookie") || !strings.Contains(item.Error, "login") {
		t.Fatalf("%s", item.Error)
	}
}
func TestRemoveQueuedTask(t *testing.T) {
	b := helper(t)
	t.Setenv("WIRE_VIDEO_SLOW", "1")
	specs, err := b.Resolve(context.Background(), "https://x.com/a/status/123")
	if err != nil {
		t.Fatal(err)
	}
	for i, spec := range specs {
		if err = b.Restore(fmt.Sprint(i), spec, engine.Item{}); err != nil {
			t.Fatal(err)
		}
	}
	if err = b.Remove(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	await(t, b, "1", "removed")
	if err = b.Resume(context.Background(), "1"); err == nil {
		t.Fatal("resumed removed task")
	}
}

func TestHLSFloatingPointProgress(t *testing.T) {
	b := helper(t)
	task := &task{item: engine.Item{Status: "resolving"}}
	b.progress(task, []byte(`{"status":"downloading","downloaded_bytes":924,"total_bytes":null,"total_bytes_estimate":1848.0,"speed":853.83}`))
	if task.item.Status != "active" || task.item.Progress != 50 || task.item.Completed != 924 || task.item.DownloadRate != 853 {
		t.Fatalf("HLS progress lost: %+v", task.item)
	}
}

func TestCookieExportIsolationAndCleanup(t *testing.T) {
	b := helper(t)
	session := auth.Session{Origin: "https://twitter.com", UserAgent: "test", Cookies: []auth.Cookie{{Name: "auth_token", Value: "private-value", Domain: ".twitter.com", Path: "/", Secure: true}}}
	if err := auth.Save(b.opts.StateDir, session); err != nil {
		t.Fatal(err)
	}
	cmd, cleanup, err := b.command(context.Background(), "https://x.com/i/web/status/123", "--version")
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for i, arg := range cmd.Args {
		if arg == "--cookies" {
			path = cmd.Args[i+1]
		}
	}
	if path == "" || strings.Contains(strings.Join(cmd.Args, " "), "private-value") {
		t.Fatal("invalid cookie arguments")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("cookie file permissions", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), ".x.com\tTRUE") || strings.Contains(string(data), "twitter.com") {
		t.Fatalf("cookie domain export: %s %v", data, err)
	}
	cleanup()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cookie export remains")
	}
	cmd, cleanup, err = b.command(context.Background(), "https://www.youtube.com/watch?v=BaW_jenozKc", "--version")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, arg := range cmd.Args {
		if arg == "--cookies" {
			t.Fatal("X cookies exported to YouTube")
		}
	}
}
