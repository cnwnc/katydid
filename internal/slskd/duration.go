package slskd

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration decodes slskd durations, which serialize as .NET timespan
// strings ("01:02:03") and may also be numeric seconds.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	value := strings.Trim(string(bytes.TrimSpace(data)), `"`)
	if value == "" || value == "null" {
		return nil
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		d.Duration = time.Duration(seconds) * time.Second
		return nil
	}
	parsed, err := parseTimeSpan(value)
	if err != nil {
		return fmt.Errorf("parse slskd duration %q: %w", value, err)
	}
	d.Duration = parsed
	return nil
}

// parseTimeSpan reads .NET timespan strings: [-]d.hh:mm:ss.fff,
// hh:mm:ss.fff, or mm:ss.fff.
func parseTimeSpan(value string) (time.Duration, error) {
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = value[1:]
	}
	parts := strings.Split(value, ":")
	if len(parts) > 4 || len(parts) < 1 {
		return 0, fmt.Errorf("unsupported timespan %q", value)
	}
	seconds := 0.0
	last := parts[len(parts)-1]
	if _, err := fmt.Sscanf(last, "%f", &seconds); err != nil {
		return 0, fmt.Errorf("parse timespan seconds %q: %w", last, err)
	}
	var minutes, hours, days int
	if len(parts) >= 2 {
		if m, err := strconv.Atoi(parts[len(parts)-2]); err == nil {
			minutes = m
		}
	}
	if len(parts) >= 3 {
		if h, err := strconv.Atoi(parts[len(parts)-3]); err == nil {
			hours = h
		}
	}
	if len(parts) == 4 {
		if d, err := strconv.Atoi(parts[0]); err == nil {
			days = d
		}
	}
	total := time.Duration(days)*24*time.Hour +
		time.Duration(hours)*time.Hour +
		time.Duration(minutes)*time.Minute +
		time.Duration(seconds*float64(time.Second))
	if negative {
		return -total, nil
	}
	return total, nil
}
