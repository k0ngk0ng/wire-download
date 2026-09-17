package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Config struct {
	Version        int      `json:"version"`
	Downloads      string   `json:"downloads"`
	Aria2Binary    string   `json:"aria2_binary"`
	AMuledBinary   string   `json:"amuled_binary"`
	AMulecmdBinary string   `json:"amulecmd_binary"`
	Aria2Port      int      `json:"aria2_port"`
	AMulePort      int      `json:"amule_ec_port"`
	AuthProxyPort  int      `json:"auth_proxy_port"`
	BTPort         int      `json:"bt_port"`
	ED2KPort       int      `json:"ed2k_port"`
	KadPort        int      `json:"kad_port"`
	Secret         string   `json:"secret"`
	MaxDownloads   int      `json:"max_downloads"`
	DownloadLimit  string   `json:"download_limit"`
	UploadLimit    string   `json:"upload_limit"`
	SeedRatio      float64  `json:"seed_ratio"`
	Trackers       []string `json:"trackers"`
	ServerListURLs []string `json:"server_list_urls"`
}

func Init(dir, downloads string) (Config, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Config{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		return Config{}, errors.New("config already exists; refusing to overwrite")
	}
	absolute, err := filepath.Abs(downloads)
	if err != nil {
		return Config{}, err
	}
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return Config{}, err
	}
	c := Config{Version: 1, Downloads: absolute, Aria2Binary: "aria2c", AMuledBinary: "amuled", AMulecmdBinary: "amulecmd", Aria2Port: 16800, AMulePort: 14712, BTPort: 16881, ED2KPort: 14662, KadPort: 14672, Secret: hex.EncodeToString(b), MaxDownloads: 5, DownloadLimit: "0", UploadLimit: "1M", SeedRatio: 1.0,
		Trackers:       []string{"udp://tracker.opentrackr.org:1337/announce", "udp://open.stealth.si:80/announce", "udp://tracker.torrent.eu.org:451/announce", "https://tracker.tamersunion.org:443/announce"},
		ServerListURLs: []string{"https://upd.emule-security.org/server.met", "https://www.gruk.org/server.met", "https://emule.shortypower.org/server.met"}}
	c.AuthProxyPort = 16802
	if err = c.Validate(); err != nil {
		return Config{}, err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	data = append(data, '\n')
	f, err := os.OpenFile(filepath.Join(dir, "config.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Config{}, err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return Config{}, err
	}
	return c, closeErr
}

func Load(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return c, fmt.Errorf("read config (run wirectl download init first): %w", err)
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.AuthProxyPort == 0 {
		c.AuthProxyPort = 16802
	}
	c.Aria2Binary = ResolveBinary(c.Aria2Binary)
	c.AMuledBinary = ResolveBinary(c.AMuledBinary)
	c.AMulecmdBinary = ResolveBinary(c.AMulecmdBinary)
	return c, c.Validate()
}

// ResolveBinary prefers engines shipped with this installation. Explicit paths
// in config remain available for development and controlled engine upgrades.
func ResolveBinary(name string) string {
	if filepath.Base(name) != name {
		return name
	}
	self, err := os.Executable()
	if err != nil {
		return name
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	bundled := filepath.Join(filepath.Dir(self), "..", "libexec", "wirectl-download", "bin", name)
	if st, err := os.Stat(bundled); err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
		return filepath.Clean(bundled)
	}
	return name
}
func (c Config) Validate() error {
	if c.Version != 1 {
		return errors.New("unsupported config version")
	}
	if !filepath.IsAbs(c.Downloads) {
		return errors.New("downloads must be an absolute path")
	}
	if len(c.Secret) < 32 {
		return errors.New("secret must contain at least 32 characters")
	}
	ports := map[int]bool{}
	for _, p := range []int{c.Aria2Port, c.AMulePort, c.BTPort, c.ED2KPort, c.KadPort, c.AuthProxyPort} {
		if p < 1024 || p > 65532 || ports[p] {
			return errors.New("ports must be unique and between 1024 and 65532")
		}
		ports[p] = true
	}
	if ports[c.ED2KPort+3] {
		return errors.New("ed2k_port + 3 must not conflict with another configured port")
	}
	if c.MaxDownloads < 1 || c.MaxDownloads > 1000 {
		return errors.New("max_downloads must be 1..1000")
	}
	if c.SeedRatio < 0 {
		return errors.New("seed_ratio cannot be negative")
	}
	for _, v := range []string{c.DownloadLimit, c.UploadLimit} {
		if _, err := RateBytes(v); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTracker checks the URL forms accepted by aria2's bt-tracker
// option.  Trackers are persisted in config.json and later joined into a
// comma-separated engine option, so control characters and empty hosts are
// rejected before they reach the generated aria2 configuration.
func ValidateTracker(raw string) error {
	if len(raw) == 0 || len(raw) > 4096 {
		return errors.New("tracker URL must be 1–4096 bytes")
	}
	if !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return errors.New("tracker URL contains invalid characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return errors.New("tracker URL must include a host")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "udp":
		return nil
	default:
		return errors.New("tracker URL must use http, https, or udp")
	}
}

func RateBytes(value string) (int64, error) {
	unit := int64(1)
	if strings.HasSuffix(value, "K") {
		unit = 1024
		value = strings.TrimSuffix(value, "K")
	} else if strings.HasSuffix(value, "M") {
		unit = 1024 * 1024
		value = strings.TrimSuffix(value, "M")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("rate limit must be a number optionally followed by K or M")
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 || n > 65535*1024/unit {
		return 0, errors.New("rate limit must be 0 (unlimited) or at most 65535 KiB/s")
	}
	return n * unit, nil
}
