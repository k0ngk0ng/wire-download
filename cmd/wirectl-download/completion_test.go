package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func captureCompletionOutput(t *testing.T, args ...string) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = writer
	dataCh := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		dataCh <- data
	}()
	callErr := completionCommand(args)
	closeErr := writer.Close()
	os.Stdout = oldStdout
	data := <-dataCh
	_ = reader.Close()
	if callErr == nil && closeErr != nil {
		callErr = closeErr
	}
	return string(data), callErr
}

func TestCompletionCommandDoesNotNeedConfiguration(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		shell := shell
		t.Run(shell, func(t *testing.T) {
			output, err := captureCompletionOutput(t, shell)
			if err != nil {
				t.Fatalf("completionCommand(%q): %v", shell, err)
			}
			if output == "" || !strings.Contains(output, "wirectl-download") {
				t.Fatalf("completionCommand(%q) returned no usable script", shell)
			}
		})
	}

	if output, err := captureCompletionOutput(t); err == nil || output != "" {
		t.Fatalf("missing shell: output=%q err=%v", output, err)
	}
	if output, err := captureCompletionOutput(t, "powershell"); err == nil || output != "" {
		t.Fatalf("unsupported shell: output=%q err=%v", output, err)
	}
}

func TestCompletionScriptsParse(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		shell := shell
		t.Run(shell, func(t *testing.T) {
			shellPath, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is not installed", shell)
			}
			output, err := completionScript(shell)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"-n"}
			if shell == "fish" {
				args = nil
			}
			cmd := exec.Command(shellPath, args...)
			cmd.Stdin = strings.NewReader(output)
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s syntax: %v\n%s", shell, err, data)
			}
		})
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func runBashCompletion(t *testing.T, words []string, current int) []string {
	t.Helper()
	script, err := completionScript("bash")
	if err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = shellQuote(word)
	}
	program := script + "\nCOMP_WORDS=(" + strings.Join(quoted, " ") + ")\nCOMP_CWORD=" + strconv.Itoa(current) + "\n_wirectl_download_completion\nprintf '%s\\n' \"${COMPREPLY[@]}\"\n"
	cmd := exec.Command("bash", "-c", program)
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash completion: %v\n%s", err, data)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func containsCompletion(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestBashCompletionContexts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	tests := []struct {
		name  string
		words []string
		at    int
		want  string
	}{
		{name: "standalone command", words: []string{"wirectl-download", "se"}, at: 1, want: "search"},
		{name: "root dispatch", words: []string{"wirectl", "dow"}, at: 1, want: "download"},
		{name: "data dir value is skipped", words: []string{"wirectl-download", "--data-dir", ".cache", "se"}, at: 3, want: "search"},
		{name: "result type enum", words: []string{"wirectl-download", "search", "--type", "to"}, at: 3, want: "torrent"},
		{name: "ed2k mode enum", words: []string{"wirectl-download", "search", "--ed2k-mode", "s"}, at: 3, want: "server"},
		{name: "nested source action", words: []string{"wirectl-download", "search", "sources", "d"}, at: 3, want: "disable"},
		{name: "eMule server action", words: []string{"wirectl-download", "emule", "servers", "l"}, at: 3, want: "list"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := runBashCompletion(t, test.words, test.at)
			if !containsCompletion(values, test.want) {
				t.Fatalf("completion=%q, want %q", values, test.want)
			}
		})
	}
	t.Run("path", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "fixture file.torrent")
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		prefix := filepath.Join(dir, "fixture ")
		values := runBashCompletion(t, []string{"wirectl-download", "add", prefix}, 2)
		if !containsCompletion(values, path) {
			t.Fatalf("completion=%q, want %q", values, path)
		}
	})
}

func TestBashDirectoryCompletion(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "completion-test")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(dir, "completion-")
	values := runBashCompletion(t, []string{"wirectl-download", "--data-dir", prefix}, 2)
	if !containsCompletion(values, path) {
		t.Fatalf("directory completion=%q, want %q", values, path)
	}
}
