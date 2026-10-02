package main

import (
	"testing"

	"doppel.moe/katydid/internal/cli"
)

func TestDedupeIndexIgnoresFailedWants(t *testing.T) {
	wants := []cli.WantReply{
		{ID: "w1", Artist: "a", Album: "b", State: "downloading"},
		{ID: "w2", Artist: "c", Album: "d", State: "failed"},
		{ID: "w3", Artist: "e", Album: "f", State: "imported", ReleaseArtist: "e", ReleaseTitle: "f"},
		{ID: "w4", Artist: "g", Album: "h", State: "downloading", ReleaseArtist: "g", ReleaseTitle: "h"},
	}
	typed, byRelease := dedupeIndex(wants)
	if _, ok := typed[wantKey("a", "b")]; !ok {
		t.Fatalf("active want should be in the typed index: %v", typed)
	}
	if _, ok := typed[wantKey("c", "d")]; ok {
		t.Fatalf("a failed want must not block re-adding: %v", typed)
	}
	if _, ok := byRelease[wantKey("c", "d")]; ok {
		t.Fatalf("a failed want must not block by release either: %v", byRelease)
	}
	if _, ok := byRelease[wantKey("g", "h")]; !ok {
		t.Fatalf("active want with resolved names should be indexed: %v", byRelease)
	}
	if _, ok := byRelease[wantKey("e", "f")]; !ok {
		t.Fatalf("imported want should keep the skip: %v", byRelease)
	}
}
