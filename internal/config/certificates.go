package config

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
)

// FallbackCAFile supplies trust roots for minimal Linux systems. Explicit user
// trust settings and installed system bundles always take precedence.
func FallbackCAFile() string {
	if runtime.GOOS != "linux" || os.Getenv("SSL_CERT_FILE") != "" || os.Getenv("SSL_CERT_DIR") != "" {
		return ""
	}
	for _, path := range []string{
		"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem", "/etc/pki/tls/cacert.pem", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"/etc/ssl/cert.pem",
	} {
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			return ""
		}
	}
	if roots, err := x509.SystemCertPool(); err == nil && len(roots.Subjects()) > 0 {
		return ""
	}
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	path := filepath.Join(filepath.Dir(self), "..", "libexec", "wirectl-download", "certs", "ca-certificates.crt")
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
		return path
	}
	return ""
}
