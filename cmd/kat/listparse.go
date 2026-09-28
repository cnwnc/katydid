package main

import (
	"regexp"
	"strconv"
	"strings"
)

// listLine is one kept line of an album list. Number is the 1-based
// file line number; Raw is the trimmed original line; Year is a
// leading year like "1979 Brian Eno - X" when present.
type listLine struct {
	Number int
	Raw    string
	Year   int
	Artist string
	Album  string
}

// trackPrefix matches a leading track number such as "01. " or "3) ".
var trackPrefix = regexp.MustCompile(`^[0-9]{1,3}[.)-]\s+`)

// yearPrefix matches a leading year such as "1979 Brian Eno - X" or
// "(1979) Brian Eno - X"; a plausible year, not any four digits.
var yearPrefix = regexp.MustCompile(`^\(?([12][0-9]{3})\)?[.:\-–—]*\s+`)

// separators split artist from album; the first occurrence wins.
var separators = []string{" - ", " – ", " — "}

// parseAlbumList splits raw into lines and takes artist and album from
// each. Blank lines and # comments are skipped; a line without a
// separator keeps empty Artist and Album for the caller to reject.
func parseAlbumList(raw string) []listLine {
	var out []listLine
	for i, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, splitListLine(i+1, line))
	}
	return out
}

// splitListLine strips the leading track number and year, then splits
// at the first separator.
func splitListLine(number int, line string) listLine {
	l := listLine{Number: number, Raw: line}
	body := trackPrefix.ReplaceAllString(line, "")
	if m := yearPrefix.FindStringSubmatch(body); m != nil {
		l.Year, _ = strconv.Atoi(m[1])
		body = strings.TrimSpace(body[len(m[0]):])
	}
	for _, sep := range separators {
		if k := strings.Index(body, sep); k >= 0 {
			l.Artist = strings.TrimSpace(body[:k])
			l.Album = strings.TrimSpace(body[k+len(sep):])
			return l
		}
	}
	return l
}
