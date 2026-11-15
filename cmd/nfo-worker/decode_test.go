package main

import (
	"testing"
	"unicode/utf8"
)

func TestIsNFOName(t *testing.T) {
	if !isNFOName("release.nfo") || !isNFOName("X.NFO") {
		t.Fatal("expected match")
	}
	if isNFOName("file.txt") || isNFOName("nfo") {
		t.Fatal("expected reject")
	}
}

func TestDecodeNFOBytes_utf8Passthrough(t *testing.T) {
	in := []byte("hello café")
	text, enc := decodeNFOBytes(in)
	if enc != "utf8" || text != string(in) {
		t.Fatalf("%q %s", text, enc)
	}
}

func TestDecodeNFOBytes_cp437(t *testing.T) {
	// 0xB3 is box-drawing vertical in CP437; invalid as lone UTF-8 byte.
	in := []byte{0x48, 0x69, 0x20, 0xb3, 0x0a} // "Hi │\n"
	if utf8.Valid(in) {
		t.Fatal("fixture should be invalid utf8")
	}
	text, enc := decodeNFOBytes(in)
	if enc != "cp437" {
		t.Fatalf("enc=%s", enc)
	}
	if !stringsContainsRune(text, '\u2502') {
		t.Fatalf("missing box draw: %q", text)
	}
}

func stringsContainsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
