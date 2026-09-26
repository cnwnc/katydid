// Package ansi renders terminal color escapes, on only when stdout is a
// terminal and NO_COLOR is unset.
package ansi

import (
	"os"
	"strconv"
	"sync"
)

var detect = sync.OnceValue(func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
})

var enabled = detect()

// SetEnabled overrides detection; meant for tests.
func SetEnabled(v bool) { enabled = v }

func Enabled() bool { return enabled }

func Dim(s string) string    { return wrap(2, s) }
func Bold(s string) string   { return wrap(1, s) }
func Red(s string) string    { return wrap(31, s) }
func Green(s string) string  { return wrap(32, s) }
func Yellow(s string) string { return wrap(33, s) }
func Cyan(s string) string   { return wrap(36, s) }

func wrap(code int, s string) string {
	if !enabled || s == "" {
		return s
	}
	return "\x1b[" + strconv.Itoa(code) + "m" + s + "\x1b[0m"
}
