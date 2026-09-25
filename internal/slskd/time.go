package slskd

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

// Time decodes slskd timestamps, which may be timezone-naive
// (2026-06-25T01:41:01.7305916) or carry an offset
// (2026-06-25T01:41:01Z); naive values are read as UTC.
type Time struct {
	time.Time
}

func (t *Time) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if string(trimmed) == "null" {
		return nil
	}
	value := strings.Trim(string(trimmed), `"`)
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		t.Time = parsed
		return nil
	}
	layouts := []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	var lastErr error
	for _, layout := range layouts {
		parsed, lastErr = time.Parse(layout, value)
		if lastErr == nil {
			t.Time = parsed.UTC()
			return nil
		}
	}
	return fmt.Errorf("parse slskd time %q: %w", value, lastErr)
}
