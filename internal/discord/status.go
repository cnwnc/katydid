package discord

import (
	"fmt"
	"strings"
)

const (
	stateQueued        = "queued"
	stateNeedsRelease  = "needs_release"
	stateSearching     = "searching"
	stateDownloading   = "downloading"
	stateNeedsDecision = "needs_decision"
	stateImported      = "imported"
	stateSkipped       = "skipped"
	stateFailed        = "failed"

	notesTailSize = 2
	wantNoteLimit = 5
	listLineLimit = 30
)

func terminalState(state string) bool {
	return state == stateImported || state == stateSkipped || state == stateFailed
}

// statusLine renders the poll status update for a want.
func statusLine(w Want) string {
	var b strings.Builder
	switch w.State {
	case stateImported:
		b.WriteString("Imported: " + w.Label())
	case stateSkipped:
		b.WriteString("Skipped " + w.Label() + ".")
	case stateFailed:
		if w.Error != "" {
			b.WriteString("Failed " + w.Label() + ": " + w.Error)
		} else {
			b.WriteString("Failed " + w.Label() + ".")
		}
		b.WriteString("\nWant " + w.ID + " — retry with /addalbum or inspect with /want.")
	case stateNeedsDecision:
		b.WriteString("Needs a manual decision — run `kat decisions` / `kat decide <token>`; token: " + w.DecisionToken)
	case stateDownloading:
		b.WriteString(w.Label() + ": " + w.State)
		if len(w.Enqueued) > 0 {
			fmt.Fprintf(&b, " (%d/%d)", w.Downloaded, len(w.Enqueued))
		}
	default:
		b.WriteString(w.Label() + ": " + w.State)
	}
	if tail := notesTail(w.Notes, notesTailSize); tail != "" {
		b.WriteString("\n" + tail)
	}
	if w.Error != "" && w.State != stateFailed {
		b.WriteString("\nerror: " + w.Error)
	}
	return b.String()
}

// timeoutLine closes the poll when the interaction token is about to expire.
func timeoutLine(wantID string) string {
	return "Still in progress after 14m — check /wants or inspect with /want id:" + wantID
}

func notesTail(notes []string, n int) string {
	if len(notes) > n {
		notes = notes[len(notes)-n:]
	}
	lines := make([]string, 0, len(notes))
	for _, note := range notes {
		lines = append(lines, "• "+note)
	}
	return strings.Join(lines, "\n")
}

// listText renders the /wants listing.
func listText(wants []Want) string {
	if len(wants) == 0 {
		return "No wants queued."
	}
	lines := make([]string, 0, listLineLimit)
	for _, w := range wants {
		lines = append(lines, fmt.Sprintf("%s %s %s", w.ID, w.State, w.Label()))
	}
	if len(lines) > listLineLimit {
		return strings.Join(lines[:listLineLimit], "\n") + fmt.Sprintf("\n…and %d more", len(wants)-listLineLimit)
	}
	return strings.Join(lines, "\n")
}

// wantText renders the /want detail view.
func wantText(w Want) string {
	var b strings.Builder
	b.WriteString(w.Label())
	if w.Year != 0 {
		fmt.Fprintf(&b, " (%d)", w.Year)
	}
	fmt.Fprintf(&b, "\nid: %s\nstate: %s", w.ID, w.State)
	if w.AlbumID != "" {
		fmt.Fprintf(&b, "\nalbum: %s", w.AlbumID)
	}
	if w.Error != "" {
		fmt.Fprintf(&b, "\nerror: %s", w.Error)
	}
	if w.DecisionToken != "" {
		fmt.Fprintf(&b, "\ntoken: %s", w.DecisionToken)
	}
	notes := w.Notes
	if len(notes) > wantNoteLimit {
		fmt.Fprintf(&b, "\n…%d earlier notes", len(notes)-wantNoteLimit)
		notes = notes[len(notes)-wantNoteLimit:]
	}
	for _, note := range notes {
		fmt.Fprintf(&b, "\n• %s", note)
	}
	return b.String()
}

// queuedLine renders the pick confirmation before the want id exists.
func queuedLine(spec addSpec, label string) string {
	if label == "" {
		label = spec.Artist + " - " + spec.Album
	}
	return "Queued " + label + ": resolving…"
}
