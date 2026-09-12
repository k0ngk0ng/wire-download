package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitPrivateAndNonDestructive(t *testing.T) {
	dir := t.TempDir()
	c, err := Init(dir, filepath.Join(dir, "downloads"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Secret == "" {
		t.Fatal("missing secret")
	}
	st, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
	if _, err = Init(dir, "ignored"); err == nil {
		t.Fatal("existing config overwritten")
	}
	loaded, err := Load(dir)
	if err != nil || loaded.Secret != c.Secret {
		t.Fatal(err)
	}
}
func TestRateBytes(t *testing.T) {
	for input, want := range map[string]int64{"0": 0, "1M": 1048576, "10K": 10240, "1024": 1024} {
		got, err := RateBytes(input)
		if err != nil || got != want {
			t.Fatal(input, got, err)
		}
	}
	for _, input := range []string{"", "K", "+1", "-1", "1G", "999999999999999999999", "1000M"} {
		if _, err := RateBytes(input); err == nil {
			t.Fatal("accepted", input)
		}
	}
}
