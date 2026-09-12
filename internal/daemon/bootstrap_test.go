package daemon

import (
	"encoding/binary"
	"os"
	"testing"
)

func TestServerMetValidation(t *testing.T) {
	b := make([]byte, 15)
	b[0] = 0xe0
	binary.LittleEndian.PutUint32(b[1:5], 1)
	copy(b[5:9], []byte{1, 2, 3, 4})
	binary.LittleEndian.PutUint16(b[9:11], 4661)
	if err := ValidateServerMet(b); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(b); n++ {
		if err := ValidateServerMet(b[:n]); err == nil {
			t.Fatalf("truncation %d accepted", n)
		}
	}
	b = append(b, 0)
	if err := ValidateServerMet(b); err == nil {
		t.Fatal("trailing data accepted")
	}
}
func TestDownloadedServerMet(t *testing.T) {
	path := os.Getenv("WIRECTL_TEST_SERVER_MET")
	if path == "" {
		t.Skip("optional live downloaded server.met fixture")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateServerMet(b); err != nil {
		t.Fatal(err)
	}
}
func FuzzServerMet(f *testing.F) {
	f.Add([]byte{0xe0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) { _ = ValidateServerMet(b) })
}
