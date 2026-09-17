package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const defaultAMulePort = 4712

var (
	errAMuleClosed    = errors.New("aMule backend is closed")
	errAMuleNoPass    = errors.New("aMule external connection password is empty")
	errAMuleNoCommand = errors.New("aMule command is empty")
)

// amuleCommandExecutor is deliberately a small function type.  Apart from
// making the adapter straightforward to test, keeping command execution
// behind this boundary makes it impossible for the adapter to accidentally
// grow a shell invocation: the production implementation calls exec.Command
// directly with an argv slice.
type amuleCommandExecutor func(context.Context, string, ...string) ([]byte, error)

// AMule controls an amuled instance through its External Connections client.
// Each operation starts a short-lived amulecmd process.  The daemon itself is
// therefore independent of the wire-download process and can be supervised by
// systemd, Docker, or another process manager.
//
// password is the plain External Connections password expected by amulecmd's
// --password option.  amulecmd hashes that value before performing EC
// authentication.  When configDir/remote.conf exists, the adapter uses that
// file instead and does not put the secret in the process argument list;
// remote.conf stores the MD5 form under /EC/Password.
type AMule struct {
	binary    string
	configDir string
	password  string
	host      string
	port      int

	mu       sync.RWMutex
	closed   bool
	executor amuleCommandExecutor
	// searchGate serializes native EC searches for one core. aMule exposes
	// only one current search/result context (0xffffffff), so overlapping
	// searches would overwrite each other's results. It is deliberately
	// independent from mu: ordinary queue operations remain available while a
	// search is running.
	searchGate chan struct{}
}

// NewAMule creates an External Connections adapter.  The optional port keeps
// compatibility with the original four-argument API while allowing callers
// to use a non-default EC port.  A missing, zero, negative, or out-of-range
// port uses aMule's documented default of 4712.
func NewAMule(binary, configDir, password string, ports ...int) *AMule {
	port := defaultAMulePort
	if len(ports) > 0 && ports[0] > 0 && ports[0] <= 65535 {
		port = ports[0]
	}
	if binary == "" {
		binary = "amulecmd"
	}
	return &AMule{
		binary:     binary,
		configDir:  configDir,
		password:   password,
		host:       "127.0.0.1",
		port:       port,
		executor:   defaultAMuleCommandExecutor,
		searchGate: make(chan struct{}, 1),
	}
}

// Connect asks the remote core to connect to its enabled networks.  amulecmd
// has no persistent connection API; the one-shot command still exercises the
// complete EC login path and the daemon keeps the resulting network session.
// This method is intentionally an extra method on AMule rather than a Backend
// requirement so callers can use it when they need a readiness check.
func (a *AMule) Connect(ctx context.Context) error {
	_, err := a.execute(ctx, "connect")
	return err
}

// ShowServers returns the text produced by amulecmd's `Show Servers`
// command.  The response is intentionally kept as raw bytes so the CLI can
// preserve aMule's server names and table formatting exactly as displayed by
// the text client.
func (a *AMule) ShowServers(ctx context.Context) ([]byte, error) {
	return a.execute(ctx, "show servers")
}

// Add queues an eD2k or magnet link.  amulecmd acknowledges a successful add
// with only "Operation was successful." and does not return the newly created
// queue item's id.  File links contain the eD2k hash, so that hash is returned
// as the stable item id.  For server-list links (which do not create a queue
// item), the original link is returned after a successful command.
func (a *AMule) Add(ctx context.Context, link string) (string, error) {
	if err := validAMuleText(link, "link"); err != nil {
		return "", err
	}
	out, err := a.execute(ctx, "add "+link)
	if err != nil {
		return "", err
	}
	if !bytes.Contains(out, []byte("Operation was successful.")) {
		return "", a.sanitizeError(fmt.Errorf("aMule did not acknowledge the download: %s", strings.TrimSpace(string(out))))
	}
	if id := amuleLinkID(link); id != "" {
		return id, nil
	}
	return link, nil
}

// List returns active downloads and completed files.  amulecmd's show dl text
// format exposes a percentage, source counts, status, part-met name, priority,
// and (only while transferring) a speed.  It does not expose byte totals, so
// Total and Completed remain zero while Progress and DownloadRate are
// populated when available.  Completed files are recovered from show shared:
// a shared entry without the [PartFile] marker is a finished file.  Download
// entries take precedence when a hash occurs in both responses.
func (a *AMule) List(ctx context.Context) ([]Item, error) {
	downloadOutput, err := a.execute(ctx, "show dl")
	if err != nil {
		return nil, err
	}
	sharedOutput, err := a.execute(ctx, "show shared")
	if err != nil {
		return nil, err
	}
	return mergeAMuleItems(parseAMuleDownloads(downloadOutput), parseAMuleShared(sharedOutput)), nil
}

// Pause pauses an item identified by its hash, numeric queue index, or other
// single-token id accepted by amulecmd.  Add returns hashes for file links,
// which avoids the ambiguity of filenames containing spaces.
func (a *AMule) Pause(ctx context.Context, id string) error {
	id, err := validAMuleID(id)
	if err != nil {
		return err
	}
	_, err = a.execute(ctx, "pause "+id)
	return err
}

// Resume resumes an item identified by its hash, numeric queue index, or
// other single-token id accepted by amulecmd.
func (a *AMule) Resume(ctx context.Context, id string) error {
	id, err := validAMuleID(id)
	if err != nil {
		return err
	}
	_, err = a.execute(ctx, "resume "+id)
	return err
}

// Remove maps the generic backend operation to amulecmd's Cancel command.
// Cancel removes the part file from aMule's download queue.
func (a *AMule) Remove(ctx context.Context, id string) error {
	id, err := validAMuleID(id)
	if err != nil {
		return err
	}
	_, err = a.execute(ctx, "cancel "+id)
	return err
}

// Close makes subsequent operations fail quickly.  There is no persistent
// socket to close because every operation owns its amulecmd subprocess.
func (a *AMule) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	return nil
}

// execute performs one command and turns both process failures and the
// textual EC failure response into errors.  amulecmd generally exits zero
// even when the remote request returns EC_OP_FAILED, so checking its output is
// required for reliable operation results.
func (a *AMule) execute(ctx context.Context, command string) ([]byte, error) {
	if a == nil {
		return nil, errAMuleClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(command) == "" {
		return nil, errAMuleNoCommand
	}

	a.mu.RLock()
	if a.closed {
		a.mu.RUnlock()
		return nil, errAMuleClosed
	}
	binary := a.binary
	host := a.host
	port := a.port
	password := a.password
	executor := a.executor
	a.mu.RUnlock()

	if executor == nil {
		executor = defaultAMuleCommandExecutor
	}
	if binary == "" {
		binary = "amulecmd"
	}
	if port <= 0 || port > 65535 {
		port = defaultAMulePort
	}

	// amulecmd's config-file option is slightly unusual: GetConfigDir only
	// honors a file beneath <cwd>/config or the platform user-data directory.
	// Pointing it at an arbitrary absolute path causes the program to prepend
	// its own user-data directory and fail to open the file.  The default
	// executor therefore runs from the parent directory and passes a relative
	// path that resolves through its portable <cwd>/config lookup.  This keeps
	// the EC MD5 password out of argv without requiring a file in $HOME.
	var configArgs []string
	if configArg, commandDir, ok := amuleRemoteConfig(a.configDir); ok {
		configArgs = []string{"--config-file=" + configArg}
		ctx = context.WithValue(ctx, amuleCommandDirKey{}, commandDir)
	}
	if len(configArgs) == 0 && strings.TrimSpace(password) == "" {
		return nil, errAMuleNoPass
	}

	// Do not add --quiet here.  aMule's quiet option suppresses Process_Answer
	// output as well as its greeting, leaving no data for List and no reliable
	// text for operation error detection.
	args := []string{
		"--host=" + host,
		"--port=" + strconv.Itoa(port),
	}
	args = append(args, configArgs...)
	if len(configArgs) == 0 {
		args = append(args, "--password="+password)
	}
	args = append(args, "--command="+command)
	out, err := executor(ctx, binary, args...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, a.sanitizeError(fmt.Errorf("amulecmd: %w", err))
	}
	if failure := amuleCommandFailure(out); failure != "" {
		return nil, a.sanitizeError(errors.New(failure))
	}
	return out, nil
}

func (a *AMule) sanitizeError(err error) error {
	if err == nil || a == nil {
		return err
	}
	a.mu.RLock()
	password := a.password
	a.mu.RUnlock()
	if password == "" {
		return err
	}
	message := strings.ReplaceAll(err.Error(), password, "[redacted]")
	return errors.New(message)
}

// defaultAMuleCommandExecutor is the only place where an OS process is
// launched.  CommandContext receives an argv vector and never interprets a
// shell expression, so links and ids cannot become shell syntax.
func defaultAMuleCommandExecutor(ctx context.Context, binary string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, defaultAMuleCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, binary, args...)
	// wxWidgets drops whole output lines containing non-ASCII filenames
	// under the C locale. Keep English messages for parsing, with UTF-8 for
	// filenames. macOS provides en_US.UTF-8; supported Linux builds provide
	// glibc's C.UTF-8 even when no additional locales have been generated.
	locale := "C.UTF-8"
	if runtime.GOOS == "darwin" {
		locale = "en_US.UTF-8"
	}
	cmd.Env = append(os.Environ(), "LC_ALL="+locale, "LANG="+locale)
	if dir, ok := runCtx.Value(amuleCommandDirKey{}).(string); ok && dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if runCtxErr := runCtx.Err(); runCtxErr != nil {
		if errors.Is(runCtxErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("amulecmd timed out after %s", defaultAMuleCommandTimeout)
		}
		return nil, runCtxErr
	}
	if stdout.truncated || stderr.truncated {
		return nil, fmt.Errorf("amulecmd output exceeded %d bytes", maxAMuleOutput)
	}
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

const defaultAMuleCommandTimeout = 30 * time.Second

// amuleCommandDirKey carries the working directory to the default executor
// without expanding the injectable executor signature used by tests.
type amuleCommandDirKey struct{}

// amuleRemoteConfig returns a portable-mode config-file argument for an
// existing configDir/remote.conf.  It never creates or modifies the file.
func amuleRemoteConfig(configDir string) (configFile, commandDir string, ok bool) {
	if strings.TrimSpace(configDir) == "" {
		return "", "", false
	}
	absDir, err := filepath.Abs(configDir)
	if err != nil {
		return "", "", false
	}
	remote := filepath.Join(absDir, "remote.conf")
	info, err := os.Stat(remote)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", false
	}
	parent := filepath.Dir(absDir)
	base := filepath.Base(absDir)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", "", false
	}
	// amulecmd resolves <cwd>/config/<config-file> before concatenating the
	// same path for the actual open.  Running in parent and using ../base makes
	// both expressions resolve to absDir/remote.conf.
	return filepath.Join("..", base, "remote.conf"), parent, true
}

// cappedBuffer prevents a malformed or unexpectedly verbose amulecmd from
// consuming unbounded memory.  64 MiB is well above normal queue output while
// still giving the caller a useful truncation error through parse behavior.
type cappedBuffer struct {
	bytes.Buffer
	truncated bool
}

const maxAMuleOutput = 64 << 20

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := maxAMuleOutput - b.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

var (
	amuleANSIRe         = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	amuleHeaderRe       = regexp.MustCompile(`(?i)^([0-9a-f]{32})[ \t]+(.*)$`)
	amuleDetailRe       = regexp.MustCompile(`(?i)\[\s*([0-9]+(?:[.,][0-9]+)?)\s*%\s*\]\s*(.*)$`)
	amuleSourcesRe      = regexp.MustCompile(`^\s*-?[0-9]+\s*/\s*-?[0-9]+\s+(?:\+\s*[0-9]+\s+)?(?:\(\s*[0-9]+\s*\)\s*-\s*|[-]\s*)?(.*)$`)
	amuleSpeedRe        = regexp.MustCompile(`(?i)([0-9]+(?:[.,][0-9]+)?)\s*([kmgt]?i?b)(?:\s*/\s*(?:s|sec))?\s*$`)
	amuleLinkHashRe     = regexp.MustCompile(`(?i)(?:\||%7c)h(?:=|%3d)([0-9a-f]{32})(?:\||%7c|$)`)
	amuleED2KSizeHashRe = regexp.MustCompile(`(?i)(?:\||%7c)[0-9]+(?:\||%7c)([0-9a-f]{32})(?:\||%7c|$)`)
	amuleMagnetHashRe   = regexp.MustCompile(`(?i)urn:ed2k:([0-9a-f]{32})`)
	amuleFailureRe      = regexp.MustCompile(`(?is)(request\s+failed\b[^\r\n]*|connection\s+failed\b[^\r\n]*|fatal\s+error\b[^\r\n]*|cannot\s+connect\b[^\r\n]*|unknown\s+command\b[^\r\n]*|error\s+processing\s+command\b[^\r\n]*)`)
	amuleSimpleIDRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// parseAMuleDownloads parses both the current TextClient output and the same
// lines without the Process_Answer " > " prefix.  Unknown localized status
// strings are retained verbatim; callers can display them without pretending
// that a locale-specific translation is a stable protocol value.
func parseAMuleDownloads(raw []byte) []Item {
	var result []Item
	var current *Item

	flush := func() {
		if current == nil {
			return
		}
		result = append(result, *current)
		current = nil
	}

	for _, rawLine := range strings.Split(string(raw), "\n") {
		line := cleanAMuleLine(rawLine)
		if line == "" {
			continue
		}
		if match := amuleHeaderRe.FindStringSubmatch(line); match != nil {
			flush()
			current = &Item{ID: strings.ToLower(match[1]), Name: strings.TrimSpace(match[2])}
			continue
		}
		if current != nil {
			parseAMuleDetail(line, current)
		}
	}
	flush()
	return result
}

func cleanAMuleLine(line string) string {
	line = strings.TrimRight(line, "\r")
	line = amuleANSIRe.ReplaceAllString(line, "")
	line = strings.TrimSpace(line)
	// Process_Answer prints every response line as " > <line>".  Remove only
	// a leading marker; a filename is otherwise allowed to contain '>'.
	if strings.HasPrefix(line, ">") {
		line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
	}
	return line
}

func parseAMuleDetail(line string, item *Item) bool {
	match := amuleDetailRe.FindStringSubmatch(line)
	if match == nil {
		return false
	}
	progress, err := strconv.ParseFloat(strings.ReplaceAll(match[1], ",", "."), 64)
	if err == nil && !math.IsNaN(progress) && !math.IsInf(progress, 0) {
		if progress < 0 {
			progress = 0
		} else if progress > 100 {
			progress = 100
		}
		// Item.Progress is a percentage across the engines (aria2 and the
		// daemon persistence layer both use 0..100), rather than a ratio.
		item.Progress = progress
	}

	rest := strings.TrimSpace(match[2])
	if sourceMatch := amuleSourcesRe.FindStringSubmatch(rest); sourceMatch != nil {
		rest = strings.TrimSpace(sourceMatch[1])
	}
	parts := splitAMuleFields(rest)
	if len(parts) > 0 {
		item.Status = strings.ToLower(strings.TrimSpace(parts[0]))
		switch item.Status {
		case "downloading":
			item.Status = "active"
		case "completed":
			item.Status = "complete"
		case "erroneous":
			item.Status = "error"
			item.Error = "aMule reports a file error; inspect its log"
		}
	}
	if len(parts) > 3 {
		if speed := parseAMuleSpeed(parts[len(parts)-1]); speed > 0 {
			item.DownloadRate = speed
		}
	} else if len(parts) > 1 {
		// Some older builds omit part-met/priority when there is no transfer;
		// still recognize a speed if it is the final field.
		if speed := parseAMuleSpeed(parts[len(parts)-1]); speed > 0 {
			item.DownloadRate = speed
		}
	}
	return true
}

func splitAMuleFields(s string) []string {
	fields := strings.Split(s, " - ")
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	for len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	return fields
}

func parseAMuleSpeed(s string) int64 {
	match := amuleSpeedRe.FindStringSubmatch(strings.TrimSpace(s))
	if match == nil {
		return 0
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(match[1], ",", "."), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	unit := strings.ToLower(match[2])
	multiplier := float64(1)
	switch unit {
	case "kb", "kib":
		multiplier = 1024
	case "mb", "mib":
		multiplier = 1024 * 1024
	case "gb", "gib":
		multiplier = 1024 * 1024 * 1024
	case "tb", "tib":
		multiplier = 1024 * 1024 * 1024 * 1024
	}
	if value > float64(math.MaxInt64)/multiplier {
		return math.MaxInt64
	}
	return int64(math.Round(value * multiplier))
}

func amuleLinkID(link string) string {
	// Prefer the file hash.  An eD2k file link may also carry one or more
	// hash extensions (|h=...), but those identify the hash set rather than
	// replacing the file's stable queue identity.
	if id := amuleED2KFileID(link); id != "" {
		return id
	}
	if match := amuleLinkHashRe.FindStringSubmatch(link); match != nil {
		return strings.ToLower(match[1])
	}
	if match := amuleMagnetHashRe.FindStringSubmatch(link); match != nil {
		return strings.ToLower(match[1])
	}
	return ""
}

func amuleED2KFileID(link string) string {
	const scheme = "ed2k://"
	if len(link) < len(scheme) || !strings.EqualFold(link[:len(scheme)], scheme) {
		return ""
	}

	body := link[len(scheme):]
	// Raw separators are unambiguous: a percent-encoded vertical bar in the
	// filename remains part of that field and cannot shift the hash index.
	if strings.HasPrefix(body, "|") {
		fields := strings.Split(body, "|")
		if len(fields) >= 5 && strings.EqualFold(fields[1], "file") {
			if id := normalizeAMuleHash(fields[4]); id != "" {
				return id
			}
		}
	}

	// Some producers encode every structural separator as %7C.  Decode only
	// the separators needed to recognize the layout; decoding the whole link
	// would turn a filename's encoded pipe into a false field boundary.
	rest, ok := consumeAMuleSeparator(body)
	if !ok || len(rest) < len("file") || !strings.EqualFold(rest[:len("file")], "file") {
		return ""
	}
	rest = rest[len("file"):]
	rest, ok = consumeAMuleSeparator(rest)
	if !ok {
		return ""
	}
	if match := amuleED2KSizeHashRe.FindStringSubmatch(rest); match != nil {
		return strings.ToLower(match[1])
	}
	return ""
}

func consumeAMuleSeparator(value string) (string, bool) {
	if strings.HasPrefix(value, "|") {
		return value[1:], true
	}
	if len(value) >= 3 && strings.EqualFold(value[:3], "%7c") {
		return value[3:], true
	}
	return "", false
}

func normalizeAMuleHash(value string) string {
	if len(value) != 32 {
		return ""
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return ""
		}
	}
	return strings.ToLower(value)
}

func amuleCommandFailure(raw []byte) string {
	match := amuleFailureRe.FindStringSubmatch(string(raw))
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// parseAMuleShared extracts completed files from `show shared`.  TextClient
// prints [PartFile] for files that are still represented by a .part file;
// those rows are deliberately ignored because the download response is the
// authoritative active state.  For ordinary shared files the command prints
// the complete path followed by the display name, so only filepath.Base is
// exposed to callers.
func parseAMuleShared(raw []byte) []Item {
	items := make([]Item, 0)
	seen := make(map[string]struct{})
	for _, rawLine := range strings.Split(string(raw), "\n") {
		line := cleanAMuleLine(rawLine)
		if line == "" {
			continue
		}
		match := amuleHeaderRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		rest := strings.TrimSpace(match[2])
		if strings.HasPrefix(strings.ToLower(rest), "[partfile]") {
			continue
		}
		name := amuleSharedBaseName(rest)
		if name == "" || name == "." || name == string(filepath.Separator) {
			continue
		}
		id := strings.ToLower(match[1])
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		items = append(items, Item{ID: id, Name: name, Status: "complete", Progress: 100})
	}
	return items
}

func amuleSharedBaseName(value string) string {
	value = strings.TrimSpace(value)
	// filepath.Base is platform-specific.  aMule can be queried remotely
	// across platforms, so normalize both separators before applying it.
	value = strings.TrimRight(value, "/\\")
	value = strings.ReplaceAll(value, "\\", "/")
	name := filepath.Base(value)
	if name == "." || name == string(filepath.Separator) || name == "/" {
		return ""
	}
	return name
}

func mergeAMuleItems(downloads, shared []Item) []Item {
	items := make([]Item, 0, len(downloads)+len(shared))
	seen := make(map[string]struct{}, len(downloads)+len(shared))
	for _, item := range append(append([]Item(nil), downloads...), shared...) {
		id := strings.ToLower(strings.TrimSpace(item.ID))
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		item.ID = id
		seen[id] = struct{}{}
		items = append(items, item)
	}
	return items
}

func validAMuleText(value, label string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("aMule %s is empty", label)
	}
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) {
			return fmt.Errorf("aMule %s contains a control character", label)
		}
	}
	return nil
}

func validAMuleID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("aMule item id is empty")
	}
	if !amuleSimpleIDRe.MatchString(value) {
		return "", errors.New("aMule item id must be a single safe token")
	}
	return value, nil
}
