package engine

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Fixed wire vector derived from aMule 2.3.3 CECTag::GetTagLen and
// WriteTag, not from this client's encoder. A parent's declared length
// excludes its own two-byte child count but includes child headers/data.
func TestECSearchStartMatchesNativeWire(t *testing.T) {
	wire, err := hex.DecodeString("2600010e03020000001200020e04060000000278000e0a06000000010000")
	if err != nil {
		t.Fatal(err)
	}
	name, _ := ecStringTag(ecTagSearchName, "x")
	kind, _ := ecStringTag(ecTagSearchFileType, "")
	tag := ecUintTag(ecTagSearchType, 0)
	tag.children = []ecTag{name, kind}
	encoded, err := (ecPacket{op: ecOpSearchStart, tags: []ecTag{tag}}).marshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, wire) {
		t.Fatalf("SEARCH_START wire mismatch\ngot  %x\nwant %x", encoded, wire)
	}
	decoded, err := parseECPacket(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.tags) != 1 || len(decoded.tags[0].children) != 2 {
		t.Fatalf("%+v", decoded)
	}
	got, err := decoded.tags[0].child(ecTagSearchName).stringValue()
	if err != nil || got != "x" {
		t.Fatalf("%q %v", got, err)
	}
}
