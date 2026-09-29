package video

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/engine"
)

// Exercise the actual packaged yt-dlp + ffmpeg pair offline. A wrapper replaces
// only extraction with fixed metadata; yt-dlp downloads two HTTP streams,
// reports progress, selects the media ID and merges with the real ffmpeg.
func TestRealVideoMerge(t *testing.T) {
	yt, ff, deno := os.Getenv("WIRECTL_TEST_YTDLP"), os.Getenv("WIRECTL_TEST_FFMPEG"), os.Getenv("WIRECTL_TEST_DENO")
	if yt == "" || ff == "" || deno == "" {
		t.Skip("set WIRECTL_TEST_YTDLP, WIRECTL_TEST_FFMPEG, WIRECTL_TEST_DENO for real video integration")
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "video.mp4")
	audio := filepath.Join(dir, "audio.m4a")
	for _, args := range [][]string{{"-f", "lavfi", "-i", "color=c=blue:s=128x96:d=2", "-an", "-c:v", "mpeg4", video}, {"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-vn", "-c:a", "aac", audio}} {
		cmd := exec.Command(ff, append([]string{"-nostdin", "-loglevel", "error"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
	}
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer server.Close()
	metadata := map[string]any{"id": "101", "title": "two tracks", "extractor": "test", "extractor_key": "Twitter", "webpage_url": server.URL, "formats": []map[string]any{
		{"format_id": "v", "url": server.URL + "/video.mp4", "ext": "mp4", "vcodec": "mpeg4", "acodec": "none", "width": 128, "height": 96},
		{"format_id": "a", "url": server.URL + "/audio.m4a", "ext": "m4a", "vcodec": "none", "acodec": "aac"},
	}}
	data, _ := json.Marshal(metadata)
	info := filepath.Join(dir, "info.json")
	if err := os.WriteFile(info, data, 0600); err != nil {
		t.Fatal(err)
	}
	// Arguments are passed verbatim using Python's execv; no shell interpolation
	// of media URLs, cookies, or output templates is involved.
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "yt-dlp-test")
	body := "#!" + python + "\nimport os,sys,subprocess\na=sys.argv[1:]\nif '--dump-single-json' in a:\n print(open(os.environ['WIRE_VIDEO_INFO_FILE']).read())\nelse:\n a=a[:a.index('--')]\n r=subprocess.run([os.environ['WIRE_REAL_YTDLP'],*a,'--load-info-json',os.environ['WIRE_VIDEO_INFO_FILE']],stderr=subprocess.PIPE)\n open(os.environ['WIRE_TEST_ERROR_LOG'],'wb').write(r.stderr)\n sys.stderr.buffer.write(r.stderr)\n sys.exit(r.returncode)\n"
	if err = os.WriteFile(wrapper, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WIRE_VIDEO_INFO_FILE", info)
	t.Setenv("WIRE_REAL_YTDLP", yt)
	t.Setenv("WIRE_TEST_ERROR_LOG", filepath.Join(dir, "stderr.log"))
	b := New(Options{Binary: wrapper, FFmpeg: ff, Deno: deno, StateDir: dir, Downloads: filepath.Join(dir, "out"), Max: 1})
	defer b.Close()
	specs, err := b.Resolve(context.Background(), "https://x.com/a/status/123")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Restore("merge", specs[0], engine.Item{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	var item engine.Item
	for time.Now().Before(deadline) {
		items, _ := b.List(context.Background())
		item = items[0]
		if item.Status == "complete" || item.Status == "error" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if item.Status != "complete" {
		log, _ := os.ReadFile(filepath.Join(dir, "stderr.log"))
		t.Fatalf("%+v\n%s", item, log)
	}
	if !strings.HasSuffix(item.Name, ".mkv") {
		t.Fatalf("streams not merged: %s", item.Name)
	}
	ffprobe := filepath.Join(filepath.Dir(ff), "ffprobe")
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_type", "-of", "json", filepath.Join(b.opts.Downloads, item.Name)).CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v %s", err, out)
	}
	var probe struct {
		Streams []struct {
			Type string `json:"codec_type"`
		} `json:"streams"`
	}
	if json.Unmarshal(out, &probe) != nil || len(probe.Streams) != 2 {
		t.Fatalf("missing audio/video: %s", out)
	}
}
