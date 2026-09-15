package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseAMuleDownloads(t *testing.T) {
	raw := []byte("" +
		"This is aMulecmd 2.4.0\n" +
		"Creating client...\n" +
		" > 0123456789abcdef0123456789abcdef Fedora Linux.iso\n" +
		" > \t [42.5%]    3/  10 + 2 ( 1) - Downloading - 001.part.met - Auto - 12.5 kB/s\n" +
		" > abcdefabcdefabcdefabcdefabcdefab Paused file.bin\n" +
		" > \t [100.0%]   10/  10     - Completed - 002.part.met - High\n")

	items := parseAMuleDownloads(raw)
	if len(items) != 2 {
		t.Fatalf("parseAMuleDownloads() returned %d items, want 2: %#v", len(items), items)
	}

	first := items[0]
	if first.ID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("first ID = %q", first.ID)
	}
	if first.Name != "Fedora Linux.iso" {
		t.Errorf("first name = %q", first.Name)
	}
	if first.Status != "active" {
		t.Errorf("first status = %q", first.Status)
	}
	if first.Progress != 42.5 {
		t.Errorf("first progress = %v, want 42.5", first.Progress)
	}
	if first.DownloadRate != 12800 {
		t.Errorf("first download rate = %d, want 12800", first.DownloadRate)
	}
	if first.Total != 0 || first.Completed != 0 {
		t.Errorf("first byte counters = %d/%d, want 0/0 because show dl exposes sources", first.Completed, first.Total)
	}

	second := items[1]
	if second.ID != "abcdefabcdefabcdefabcdefabcdefab" {
		t.Errorf("second ID = %q", second.ID)
	}
	if second.Name != "Paused file.bin" || second.Status != "complete" {
		t.Errorf("second item = %#v", second)
	}
	if second.Progress != 100 {
		t.Errorf("second progress = %v, want 100", second.Progress)
	}
}

func TestAMuleListMergesCompletedSharedFiles(t *testing.T) {
	const activeHash = "0123456789abcdef0123456789abcdef"
	const completeHash = "abcdefabcdefabcdefabcdefabcdefab"
	const partHash = "11111111111111111111111111111111"
	a := NewAMule("amulecmd", "", "secret")
	var commands []string
	a.executor = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		var command string
		for _, arg := range args {
			if strings.HasPrefix(arg, "--command=") {
				command = strings.TrimPrefix(arg, "--command=")
			}
		}
		commands = append(commands, command)
		switch command {
		case "show dl":
			return []byte(" > " + activeHash + " active.iso\n > [50.0%] 1/2 - Downloading - 001.part.met - Auto\n"), nil
		case "show shared":
			return []byte(" > " + activeHash + " /downloads/active.iso\n" +
				" > " + completeHash + " /downloads/complete.iso\n" +
				" > " + partHash + " [PartFile] still.part\n"), nil
		default:
			return nil, errors.New("unexpected command")
		}
	}

	items, err := a.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if strings.Join(commands, ",") != "show dl,show shared" {
		t.Errorf("commands = %#v, want download then shared", commands)
	}
	if len(items) != 2 {
		t.Fatalf("List() returned %d items, want 2: %#v", len(items), items)
	}
	if items[0].ID != activeHash || items[0].Status != "active" {
		t.Errorf("active item = %#v", items[0])
	}
	if items[1].ID != completeHash || items[1].Name != "complete.iso" || items[1].Status != "complete" || items[1].Progress != 100 {
		t.Errorf("completed item = %#v", items[1])
	}
}

func TestAMuleAddRejectsMissingAcknowledgement(t *testing.T) {
	a := NewAMule("amulecmd", "", "test-secret")
	a.executor = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("This is amulecmd 2.3.3\nSucceeded! Connection established to aMule 2.3.3\n"), nil
	}
	if _, err := a.Add(context.Background(), "ed2k://|file|fixture|1|bde52cb31de33e46245e05fbdbd6fb24|/"); err == nil {
		t.Fatal("a control connection is not an acknowledgement of the add operation")
	}
}

func TestAMuleCommandArgumentsAndConnect(t *testing.T) {
	a := NewAMule("/opt/amulecmd", "/var/lib/amule", "s3cret", 5678)
	var gotBinary string
	var gotArgs []string
	a.executor = func(_ context.Context, binary string, args ...string) ([]byte, error) {
		gotBinary = binary
		gotArgs = append([]string(nil), args...)
		return []byte(" > Operation was successful.\n"), nil
	}

	if err := a.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if gotBinary != "/opt/amulecmd" {
		t.Errorf("binary = %q", gotBinary)
	}
	want := []string{
		"--host=127.0.0.1",
		"--port=5678",
		"--password=s3cret",
		"--command=connect",
	}
	if strings.Join(gotArgs, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args = %#v, want %#v", gotArgs, want)
	}
	for _, arg := range gotArgs {
		if arg == "--quiet" || arg == "-q" {
			t.Error("adapter passed quiet mode, which suppresses show dl output")
		}
	}
}

func TestAMuleAddReturnsED2KHash(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef"
	a := NewAMule("amulecmd", "", "secret")
	var command string
	a.executor = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--command=") {
				command = strings.TrimPrefix(arg, "--command=")
			}
		}
		return []byte(" > Operation was successful.\n"), nil
	}

	id, err := a.Add(context.Background(), "ed2k://|file|Fedora.iso|123|"+hash+"|/")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if id != hash {
		t.Errorf("Add() id = %q, want %q", id, hash)
	}
	if command != "add ed2k://|file|Fedora.iso|123|"+hash+"|/" {
		t.Errorf("command = %q", command)
	}

	magnetID, err := a.Add(context.Background(), "magnet:?xt=urn:ed2k:"+hash+"&xl=123&dn=Fedora.iso")
	if err != nil {
		t.Fatalf("Add(magnet) error = %v", err)
	}
	if magnetID != hash {
		t.Errorf("Add(magnet) id = %q, want %q", magnetID, hash)
	}
}

func TestAMuleLinkIDHandlesEncodedED2KLinks(t *testing.T) {
	const fileHash = "0123456789abcdef0123456789abcdef"
	const extensionHash = "abcdefabcdefabcdefabcdefabcdefab"

	tests := []struct {
		name string
		link string
		want string
	}{
		{
			name: "plain",
			link: "ed2k://|file|Fedora.iso|123|" + fileHash + "|/",
			want: fileHash,
		},
		{
			name: "percent encoded filename",
			link: "ed2k://|file|%E4%B8%AD%E6%96%87.iso|123|" + fileHash + "|/",
			want: fileHash,
		},
		{
			name: "fully encoded separators",
			link: "ed2k://%7Cfile%7CFedora.iso%7C123%7C" + fileHash + "%7C/",
			want: fileHash,
		},
		{
			name: "encoded pipe in filename",
			link: "ed2k://|file|part%7Cname.iso|123|" + fileHash + "|/",
			want: fileHash,
		},
		{
			name: "file hash takes precedence over extension",
			link: "ed2k://|file|Fedora.iso|123|" + fileHash + "|h=" + extensionHash + "|/",
			want: fileHash,
		},
		{
			name: "encoded hash extension fallback",
			link: "ed2k://|server|127.0.0.1|4662%7Ch%3D" + fileHash + "%7C/",
			want: fileHash,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := amuleLinkID(tt.link); got != tt.want {
				t.Errorf("amuleLinkID(%q) = %q, want %q", tt.link, got, tt.want)
			}
		})
	}
}

func TestAMuleOperationErrorsAreDetectedAndRedacted(t *testing.T) {
	a := NewAMule("amulecmd", "", "top-secret")
	a.executor = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(" > Request failed with the following error: top-secret\n"), nil
	}
	if _, err := a.Add(context.Background(), "magnet:?xt=urn:ed2k:0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("Add() succeeded for EC failure response")
	} else if strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("Add() leaked password in error: %v", err)
	}

	a.executor = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("transport failed: top-secret")
	}
	if err := a.Pause(context.Background(), "0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("Pause() succeeded for process error")
	} else if strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("Pause() leaked password in process error: %v", err)
	}
}

func TestAMuleRejectsUnsafeArgumentsAndHonorsContext(t *testing.T) {
	a := NewAMule("amulecmd", "", "secret")
	called := false
	a.executor = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		called = true
		return []byte("Operation was successful."), nil
	}
	if _, err := a.Add(context.Background(), "magnet:?xt=urn:ed2k:0123\nconnect"); err == nil {
		t.Fatal("Add() accepted a command-injection newline")
	}
	if err := a.Remove(context.Background(), "id;shutdown"); err == nil {
		t.Fatal("Remove() accepted unsafe id")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List(canceled) error = %v, want context.Canceled", err)
	}
	if called {
		t.Error("executor called after context was canceled")
	}
}

func TestAMuleClose(t *testing.T) {
	a := NewAMule("amulecmd", "", "secret")
	if err := a.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := a.Connect(context.Background()); !errors.Is(err, errAMuleClosed) {
		t.Fatalf("Connect() after Close() error = %v, want %v", err, errAMuleClosed)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}
