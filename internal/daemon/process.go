package daemon

import (
	"context"
	"crypto/md5"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/config"
)

type process struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	log  *os.File
}

func startProcess(binary string, args []string, logPath string) (*process, error) {
	// Bound log growth across restarts; engine-native logs are separate.
	if st, err := os.Stat(logPath); err == nil && st.Size() > 8<<20 {
		if err = os.Rename(logPath, logPath+".1"); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	modules := filepath.Join(filepath.Dir(cmd.Path), "..", "lib", "ossl-modules")
	if st, err := os.Stat(modules); err == nil && st.IsDir() {
		cmd.Env = append(cmd.Env, "OPENSSL_MODULES="+modules)
	}
	// Engines remain in the daemon process group so launchd can reap them.
	if err = cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	p := &process{cmd: cmd, done: make(chan struct{}), log: f}
	go func() { p.err = cmd.Wait(); f.Close(); close(p.done) }()
	return p, nil
}
func (p *process) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}
func (p *process) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func Prepare(dir string, c config.Config) error {
	for _, d := range []string{c.Downloads, filepath.Join(dir, "config"), filepath.Join(dir, "amule"), filepath.Join(dir, "amule", "Temp"), filepath.Join(dir, "aria2"), filepath.Join(dir, "logs")} {
		if strings.ContainsAny(d, "\r\n") {
			return fmt.Errorf("paths must not contain newlines")
		}
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	for _, b := range []string{c.Aria2Binary, c.AMuledBinary, c.AMulecmdBinary} {
		if _, err := exec.LookPath(b); err != nil {
			return fmt.Errorf("missing engine %s: %w", b, err)
		}
	}
	session := filepath.Join(dir, "aria2", "session.txt")
	f, err := os.OpenFile(session, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	f.Close()
	// Save only error/unfinished downloads. force-save=true also writes
	// completed downloads to the session file; replaying those entries after a
	// restart makes aria2 see a complete file without its .aria2 control file
	// and report it as an error.
	lines := []string{"enable-rpc=true", "rpc-listen-all=false", "rpc-listen-port=" + strconv.Itoa(c.Aria2Port), "rpc-secret=" + c.Secret, "dir=" + c.Downloads, "input-file=" + session, "save-session=" + session, "save-session-interval=10", "force-save=false", "continue=true", "enable-dht=true", "enable-dht6=true", "enable-peer-exchange=true", "bt-enable-lpd=true", "bt-save-metadata=true", "follow-torrent=true", "check-integrity=true", "file-allocation=none", "max-concurrent-downloads=" + strconv.Itoa(c.MaxDownloads), "max-overall-download-limit=" + c.DownloadLimit, "max-overall-upload-limit=" + c.UploadLimit, "seed-ratio=" + strconv.FormatFloat(c.SeedRatio, 'f', 2, 64), "listen-port=" + strconv.Itoa(c.BTPort), "dht-listen-port=" + strconv.Itoa(c.BTPort), "dht-file-path=" + filepath.Join(dir, "aria2", "dht.dat"), "dht-file-path6=" + filepath.Join(dir, "aria2", "dht6.dat"), "dht-entry-point=dht.transmissionbt.com:6881", "bt-tracker=" + strings.Join(c.Trackers, ","), "bt-tracker-connect-timeout=10", "bt-tracker-timeout=10", "max-download-result=10000", "keep-unfinished-download-result=true", "auto-file-renaming=false", "allow-overwrite=false", "console-log-level=warn", "summary-interval=0"}
	if cert := config.FallbackCAFile(); cert != "" {
		lines = append(lines, "ca-certificate="+cert)
	}
	for _, line := range lines {
		if strings.ContainsAny(line, "\r\n") {
			return fmt.Errorf("invalid newline in engine configuration")
		}
	}
	if err = atomicWrite(filepath.Join(dir, "aria2", "aria2.conf"), []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		return err
	}
	// Preserve aMule's generated identity and settings while updating managed keys.
	path := filepath.Join(dir, "amule", "amule.conf")
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	hash := fmt.Sprintf("%x", md5.Sum([]byte(c.Secret))) // aMule EC requires MD5.
	if err := atomicWrite(filepath.Join(dir, "amule", "remote.conf"), []byte(fmt.Sprintf("[EC]\nHost=127.0.0.1\nPort=%d\nPassword=%s\n", c.AMulePort, hash))); err != nil {
		return err
	}
	values := map[string]map[string]string{"eMule": {"Nick": "wire-download", "MaxUpload": strconv.Itoa(amuleRate(c.UploadLimit)), "MaxDownload": strconv.Itoa(amuleRate(c.DownloadLimit)), "Reconnect": "1", "Port": strconv.Itoa(c.ED2KPort), "UDPPort": strconv.Itoa(c.KadPort), "UDPEnable": "1", "IncomingDir": c.Downloads, "TempDir": filepath.Join(dir, "amule", "Temp"), "ConnectToED2K": "1", "ConnectToKad": "1", "AddServerListFromServer": "0", "AddServerListFromClient": "0", "UPnPEnabled": "0"}, "ExternalConnect": {"AcceptExternalConnections": "1", "ECAddress": "127.0.0.1", "ECPort": strconv.Itoa(c.AMulePort), "ECPassword": hash, "UPnPECEnabled": "0"}, "WebServer": {"Enabled": "0"}}
	return atomicWrite(path, []byte(mergeINI(string(old), values)))
}
func mergeINI(old string, values map[string]map[string]string) string {
	var out strings.Builder
	section := ""
	flush := func() {
		for k, v := range values[section] {
			fmt.Fprintf(&out, "%s=%s\n", k, v)
		}
		delete(values, section)
	}
	for _, line := range strings.Split(old, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			flush()
			section = strings.Trim(trim, "[]")
			out.WriteString(line + "\n")
			continue
		}
		key, _, ok := strings.Cut(trim, "=")
		if ok {
			if _, managed := values[section][key]; managed {
				continue
			}
		}
		if line != "" {
			out.WriteString(line + "\n")
		}
	}
	flush()
	for s, entries := range values {
		fmt.Fprintf(&out, "[%s]\n", s)
		for k, v := range entries {
			fmt.Fprintf(&out, "%s=%s\n", k, v)
		}
	}
	return out.String()
}
func waitPort(ctx context.Context, p *process, port int) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !p.alive() {
			return fmt.Errorf("engine exited: %v (see logs)", p.err)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func amuleRate(limit string) int {
	n, _ := config.RateBytes(limit)
	if n == 0 {
		return 0
	}
	return max(1, int(n/1024))
}
