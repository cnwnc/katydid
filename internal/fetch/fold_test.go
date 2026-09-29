package fetch

import (
	"testing"

	"doppel.moe/katydid/internal/match"
)

func TestFoldName(t *testing.T) {
	// 弱虫モンブラン: composed (U+30D6) vs decomposed (U+30D5 + U+3099)
	nfc := "03 弱虫モンブラン.flac"
	nfd := "03 弱虫モ" + "ン" + "フ\u3099ラン.flac"
	if nfc == nfd {
		t.Fatal("fixture must differ")
	}
	// the fold itself keeps the combining mark; match's NFC composition
	// makes the pair identical
	if match.Similarity(foldName(nfc), foldName(nfd)) != 1 {
		t.Fatalf("decomposed variant should compose to identical: %q vs %q", foldName(nfc), foldName(nfd))
	}
	// fullwidth parens fold to ASCII; NBSP and ideographic space to space
	if foldName("A（R）.flac") != "a(r).flac" {
		t.Fatalf("fullwidth fold: %q", foldName("A（R）.flac"))
	}
	if foldName("A\u00A0B\u3000C") != "a b c" {
		t.Fatalf("space fold: %q", foldName("A\u00A0B\u3000C"))
	}
}
