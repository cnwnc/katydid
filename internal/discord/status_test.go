package discord

import (
	"strings"
	"testing"
)

func TestStatusLineQueued(t *testing.T) {
	w := Want{ID: "abc", Artist: "The Beatles", Album: "Abbey Road", State: stateQueued, Notes: []string{"one", "two", "three"}}
	want := "The Beatles - Abbey Road: queued\n• two\n• three"
	if got := statusLine(w); got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func TestStatusLineInProgressWithError(t *testing.T) {
	w := Want{ID: "abc", Artist: "A", Album: "B", State: stateDownloading, Error: "peer dropped"}
	want := "A - B: downloading\nerror: peer dropped"
	if got := statusLine(w); got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func TestStatusLineDownloadProgress(t *testing.T) {
	w := Want{Artist: "A", Album: "B", State: stateDownloading, Enqueued: make([]struct{}, 12), Downloaded: 5}
	if got, want := statusLine(w), "A - B: downloading (5/12)"; got != want {
		t.Fatalf("statusLine = %q, want %q", got, want)
	}
}

func TestStatusLinePrefersResolvedNames(t *testing.T) {
	w := Want{Artist: "operation sodastel", Album: "slaney vs sodastel", State: stateSearching}
	if got, want := statusLine(w), "operation sodastel - slaney vs sodastel: searching"; got != want {
		t.Fatalf("before resolve: %q, want %q", got, want)
	}
	w.ReleaseArtist, w.ReleaseTitle = "Operation Sodasteal", "SLANEY VS. SODASTEAL"
	if got, want := statusLine(w), "Operation Sodasteal - SLANEY VS. SODASTEAL: searching"; got != want {
		t.Fatalf("after resolve: %q, want %q", got, want)
	}
}

func TestStatusLineImported(t *testing.T) {
	w := Want{ID: "abc", Artist: "A", Album: "B", State: stateImported, AlbumID: "alb-1"}
	if got := statusLine(w); got != "Imported: A - B" {
		t.Fatalf("line = %q", got)
	}
	w.AlbumID = ""
	if got := statusLine(w); got != "Imported: A - B" {
		t.Fatalf("line = %q", got)
	}
	w.ReleaseArtist, w.ReleaseTitle = "Real Artist", "Real Album"
	if got := statusLine(w); got != "Imported: Real Artist - Real Album" {
		t.Fatalf("line = %q", got)
	}
}

func TestClip(t *testing.T) {
	if got := clip("short", 10); got != "short" {
		t.Fatalf("clip short = %q", got)
	}
	long := strings.Repeat("é", 2500)
	got := clip(long, messageLimit)
	if n := len([]rune(got)); n != messageLimit || !strings.HasSuffix(got, "…") {
		t.Fatalf("clip long: %d runes, suffix ok %v", n, strings.HasSuffix(got, "…"))
	}
}

func TestStatusLineSkipped(t *testing.T) {
	w := Want{ID: "abc", Artist: "A", Album: "B", State: stateSkipped}
	if got := statusLine(w); got != "Skipped A - B." {
		t.Fatalf("line = %q", got)
	}
}

func TestStatusLineFailed(t *testing.T) {
	w := Want{ID: "want-9", Artist: "A", Album: "B", State: stateFailed, Error: "no sources"}
	got := statusLine(w)
	if !strings.Contains(got, "Failed A - B: no sources") || !strings.Contains(got, "want-9") {
		t.Fatalf("line = %q", got)
	}
	w.Error = ""
	if got := statusLine(w); got != "Failed A - B.\nWant want-9 — retry with /addalbum or inspect with /want." {
		t.Fatalf("line = %q", got)
	}
}

func TestStatusLineNeedsDecision(t *testing.T) {
	w := Want{ID: "abc", Artist: "A", Album: "B", State: stateNeedsDecision, DecisionToken: "tok-1"}
	got := statusLine(w)
	if !strings.Contains(got, "kat decide <token>") || !strings.Contains(got, "tok-1") {
		t.Fatalf("line = %q", got)
	}
}

func TestTerminalState(t *testing.T) {
	for _, state := range []string{stateImported, stateSkipped, stateFailed} {
		if !terminalState(state) {
			t.Fatalf("%q should be terminal", state)
		}
	}
	for _, state := range []string{stateQueued, stateNeedsRelease, stateSearching, stateDownloading, stateNeedsDecision} {
		if terminalState(state) {
			t.Fatalf("%q should not be terminal", state)
		}
	}
}

func TestTimeoutLine(t *testing.T) {
	if got := timeoutLine("want-9"); got != "Still in progress after 14m — check /wants or inspect with /want id:want-9" {
		t.Fatalf("line = %q", got)
	}
}

func TestListText(t *testing.T) {
	if got := listText(nil); got != "No wants queued." {
		t.Fatalf("line = %q", got)
	}
	got := listText([]Want{
		{ID: "a", State: stateQueued, Artist: "A", Album: "B"},
		{ID: "c", State: stateImported, Artist: "C", Album: "D"},
	})
	want := "a queued A - B\nc imported C - D"
	if got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func TestListTextCaps(t *testing.T) {
	wants := make([]Want, 0, listLineLimit+5)
	for i := 0; i < listLineLimit+5; i++ {
		wants = append(wants, Want{ID: string(rune('a' + i)), State: stateQueued, Artist: "A", Album: "B"})
	}
	got := listText(wants)
	if got := strings.Count(got, "\n"); got != listLineLimit {
		t.Fatalf("line count = %d, want %d", got, listLineLimit)
	}
	if !strings.Contains(got, "and 5 more") {
		t.Fatalf("missing overflow line: %q", strings.Split(got, "\n")[listLineLimit])
	}
}

func TestWantText(t *testing.T) {
	w := Want{ID: "want-9", Artist: "A", Album: "B", Year: 1969, State: stateNeedsRelease, AlbumID: "alb-1", Error: "boom", DecisionToken: "tok", Notes: []string{"n1", "n2"}}
	got := wantText(w)
	for _, part := range []string{"A - B (1969)", "id: want-9", "state: needs_release", "album: alb-1", "error: boom", "token: tok", "• n1", "• n2"} {
		if !strings.Contains(got, part) {
			t.Fatalf("missing %q in %q", part, got)
		}
	}
}

func TestWantTextCapsNotes(t *testing.T) {
	w := Want{ID: "want-9", Artist: "A", Album: "B", State: stateQueued, Notes: []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7"}}
	got := wantText(w)
	if !strings.Contains(got, "2 earlier notes") || strings.Contains(got, "n2\n") {
		t.Fatalf("line = %q", got)
	}
	for _, note := range []string{"n3", "n4", "n5", "n6", "n7"} {
		if !strings.Contains(got, "• "+note) {
			t.Fatalf("missing %q in %q", note, got)
		}
	}
}

func TestQueuedLine(t *testing.T) {
	spec := addSpec{Artist: "A", Album: "B"}
	if got := queuedLine(spec, ""); got != "Queued A - B: resolving…" {
		t.Fatalf("line = %q", got)
	}
	if got := queuedLine(spec, "A - Abbey Road"); got != "Queued A - Abbey Road: resolving…" {
		t.Fatalf("line = %q", got)
	}
}
