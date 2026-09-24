// Package safe builds file and directory names from arbitrary text.
package safe

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MaxComponentBytes is the per-component budget. The kernel limit is 255
// bytes; the margin absorbs prefixes such as "2001 - " and separators.
const MaxComponentBytes = 200

// Name converts arbitrary text (artist, album, track title) into one safe
// path component. Non-latin text passes through unchanged; only bytes the
// kernel rejects and characters that break terminals or our own dot-dir
// scan are replaced. Name always NFC-normalizes so the same title always
// yields the same bytes.
func Name(name, fallback string) string {
	cleaned := norm.NFC.String(name)

	cleaned = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == 0:
			return '-'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, cleaned)

	cleaned = strings.TrimLeft(cleaned, ".")
	cleaned = strings.TrimRight(cleaned, " .")
	cleaned = strings.TrimSpace(cleaned)

	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return Name(fallback, "Unknown")
	}

	if len(cleaned) > MaxComponentBytes {
		cleaned = truncateAtRune(cleaned, MaxComponentBytes)
		cleaned = strings.TrimRight(cleaned, " .-")
	}
	return cleaned
}

func truncateAtRune(s string, limit int) string {
	for len(s) > limit {
		runes := []rune(s)
		s = string(runes[:len(runes)-1])
	}
	return s
}
