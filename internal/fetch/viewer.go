package fetch

import (
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// viewerStates is the order wants render in: active work first, then the
// states that need a human, then the terminal ones.
var viewerStates = []string{
	StateDownloading,
	StateSearching,
	StateQueued,
	StateNeedsPick,
	StateNeedsRelease,
	StateImported,
	StateSkipped,
	StateFailed,
}

// viewer renders the operator page. It is deliberately bare HTML with a
// meta refresh so a terminal browser or curl sees the same thing as a GUI.
func (s *Server) viewer(w http.ResponseWriter, r *http.Request) {
	wants := s.Orchestrator.Wants()
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n")
	b.WriteString("<meta charset=\"utf-8\">\n")
	b.WriteString("<meta http-equiv=\"refresh\" content=\"5\">\n")
	b.WriteString("<title>katydid fetchd</title>\n")
	b.WriteString("</head>\n<body>\n")
	b.WriteString("<h1>katydid fetchd</h1>\n")
	if len(wants) == 0 {
		b.WriteString("<p>no wants</p>\n")
	}
	for _, state := range viewerStates {
		group := []Want{}
		for _, want := range wants {
			if want.State == state {
				group = append(group, want)
			}
		}
		if len(group) == 0 {
			continue
		}
		b.WriteString("<h2>" + html.EscapeString(state) + "</h2>\n")
		for _, want := range group {
			viewerWant(&b, want)
		}
	}
	b.WriteString("</body>\n</html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// viewerWant writes one want as a heading, a per-owner transfer table, its
// error and recent notes, then a kill form (HTML forms cannot DELETE).
func viewerWant(b *strings.Builder, want Want) {
	title := want.ReleaseArtist + " - " + want.ReleaseTitle
	if want.ReleaseArtist == "" || want.ReleaseTitle == "" {
		title = want.Artist + " - " + want.Album
	}
	b.WriteString("<h3>" + html.EscapeString(title) + " [" + html.EscapeString(want.ID) + "]</h3>\n")
	b.WriteString("<table border=\"1\">\n")
	b.WriteString("<tr><th>peer</th><th>files</th><th>done</th><th>enqueued</th><th>place</th><th>waiting</th></tr>\n")
	type ownerCount struct {
		files int
		done  int
		open  bool
	}
	owners := map[string]*ownerCount{}
	var order []string
	for _, file := range want.Enqueued {
		owner := want.Owners[file.Filename]
		if owner == "" {
			owner = want.Peer
		}
		count := owners[owner]
		if count == nil {
			count = &ownerCount{}
			owners[owner] = count
			order = append(order, owner)
		}
		count.files++
		if _, bound := want.Paths[file.Filename]; bound {
			count.done++
		} else {
			count.open = true
		}
	}
	for _, owner := range order {
		count := owners[owner]
		enqueued := "-"
		if count.open {
			enqueued = "yes"
		}
		place := "-"
		if value, ok := want.PeerPlace[owner]; ok {
			place = strconv.FormatInt(value, 10)
		}
		waiting := "-"
		if since, ok := want.PeerProgress[owner]; ok {
			waiting = time.Since(since).Round(time.Second).String()
		}
		b.WriteString("<tr><td>" + html.EscapeString(owner) + "</td>" +
			"<td>" + strconv.Itoa(count.files) + "</td>" +
			"<td>" + strconv.Itoa(count.done) + "</td>" +
			"<td>" + enqueued + "</td>" +
			"<td>" + html.EscapeString(place) + "</td>" +
			"<td>" + html.EscapeString(waiting) + "</td></tr>\n")
	}
	b.WriteString("</table>\n")
	if want.Error != "" {
		b.WriteString("<p>" + html.EscapeString(want.Error) + "</p>\n")
	}
	notes := want.Notes
	if len(notes) > 3 {
		notes = notes[len(notes)-3:]
	}
	if len(notes) > 0 {
		quoted := make([]string, len(notes))
		for i, note := range notes {
			quoted[i] = html.EscapeString(note)
		}
		b.WriteString("<p>" + strings.Join(quoted, " | ") + "</p>\n")
	}
	switch want.State {
	case StateImported, StateSkipped, StateFailed:
		b.WriteString("<p>terminal: curl -X DELETE 'http://d/want?id=" + html.EscapeString(want.ID) + "'</p>\n")
	default:
		b.WriteString("<form method=\"POST\" action=\"/want/cancel\">\n")
		b.WriteString("<input type=\"hidden\" name=\"id\" value=\"" + html.EscapeString(want.ID) + "\">\n")
		b.WriteString("<input type=\"submit\" value=\"kill\">\n")
		b.WriteString("</form>\n")
	}
}
