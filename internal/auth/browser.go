package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type BrowserOptions struct {
	Binary   string
	Headless bool
}

func FindBrowser(explicit string) (string, error) {
	if explicit != "" {
		return exec.LookPath(explicit)
	}
	candidates := []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge"}
	if runtime.GOOS == "darwin" {
		candidates = append([]string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"}, candidates...)
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("no Chromium browser found; use --browser /path/to/chrome, or log in on your desktop with --remote user@server")
}

type devtools struct {
	conn *websocket.Conn
	next int
}

func (d *devtools) call(ctx context.Context, method string, params any, result any) error {
	d.next++
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	_ = d.conn.SetWriteDeadline(deadline)
	_ = d.conn.SetReadDeadline(deadline)
	if err := d.conn.WriteJSON(map[string]any{"id": d.next, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := d.conn.ReadJSON(&reply); err != nil {
			return err
		}
		if reply.ID != d.next {
			continue
		}
		if reply.Error != nil {
			return fmt.Errorf("browser: %s", reply.Error.Message)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(reply.Result, result)
	}
}

// Capture opens a dedicated, private browser profile. It never reads the user's
// everyday browser profile or exports unrelated websites' cookies.
func Capture(ctx context.Context, dir, target string, options BrowserOptions, confirm func() error) (Session, error) {
	origin, err := Origin(target)
	if err != nil {
		return Session{}, err
	}
	if runtime.GOOS == "linux" && !options.Headless && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return Session{}, errors.New("no graphical desktop; on your desktop run: wirectl download login <website> --remote user@server")
	}
	binary, err := FindBrowser(options.Binary)
	if err != nil {
		return Session{}, err
	}
	profile := filepath.Join(dir, "browser", strings.TrimSuffix(filepath.Base(sessionPath(dir, origin)), ".json"))
	if err = os.MkdirAll(profile, 0700); err != nil {
		return Session{}, err
	}
	lock, err := os.OpenFile(filepath.Join(profile, "login.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return Session{}, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return Session{}, errors.New("a login browser is already open for this website")
	}
	portFile := filepath.Join(profile, "DevToolsActivePort")
	if err = os.Remove(portFile); err != nil && !os.IsNotExist(err) {
		return Session{}, err
	}
	log, err := os.OpenFile(filepath.Join(profile, "browser.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return Session{}, err
	}
	defer log.Close()
	args := []string{"--user-data-dir=" + profile, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check"}
	if options.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, target)
	cmd := exec.Command(binary, args...)
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		return Session{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var endpoint string
ready:
	for {
		if data, readErr := os.ReadFile(portFile); readErr == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) == 2 {
				port, parseErr := strconv.Atoi(lines[0])
				if parseErr == nil && port > 0 && port < 65536 && strings.HasPrefix(lines[1], "/devtools/browser/") {
					endpoint = fmt.Sprintf("ws://127.0.0.1:%d%s", port, lines[1])
					break ready
				}
			}
		}
		select {
		case <-ctx.Done():
			return Session{}, ctx.Err()
		case <-timer.C:
			return Session{}, fmt.Errorf("browser startup timed out; inspect %s", log.Name())
		case <-tick.C:
		}
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return Session{}, err
	}
	defer conn.Close()
	conn.SetReadLimit(4 << 20)
	d := devtools{conn: conn}
	if err = confirm(); err != nil {
		return Session{}, err
	}
	var version struct {
		UserAgent string `json:"userAgent"`
	}
	if err = d.call(ctx, "Browser.getVersion", map[string]any{}, &version); err != nil {
		return Session{}, err
	}
	var result struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err = d.call(ctx, "Storage.getCookies", map[string]any{}, &result); err != nil {
		return Session{}, err
	}
	u, _ := url.Parse(origin)
	session := Session{Origin: origin, UserAgent: version.UserAgent}
	for _, cookie := range result.Cookies {
		domain := strings.TrimPrefix(strings.ToLower(cookie.Domain), ".")
		if domain == u.Hostname() || strings.HasSuffix(u.Hostname(), "."+domain) {
			session.Cookies = append(session.Cookies, cookie)
		}
	}
	return session, session.Validate()
}
