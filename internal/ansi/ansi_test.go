package ansi

import "testing"

func TestWrappersWhenEnabled(t *testing.T) {
	prev := Enabled()
	SetEnabled(true)
	defer SetEnabled(prev)

	cases := []struct{ got, want string }{
		{Dim("x"), "\x1b[2mx\x1b[0m"},
		{Bold("x"), "\x1b[1mx\x1b[0m"},
		{Red("x"), "\x1b[31mx\x1b[0m"},
		{Green("x"), "\x1b[32mx\x1b[0m"},
		{Yellow("x"), "\x1b[33mx\x1b[0m"},
		{Cyan("x"), "\x1b[36mx\x1b[0m"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestWrappersWhenDisabled(t *testing.T) {
	prev := Enabled()
	SetEnabled(false)
	defer SetEnabled(prev)

	for _, got := range []string{Dim("x"), Bold("x"), Red("x"), Green("x"), Yellow("x"), Cyan("x")} {
		if got != "x" {
			t.Errorf("disabled: got %q, want %q", got, "x")
		}
	}
}

func TestEmptyStaysEmpty(t *testing.T) {
	prev := Enabled()
	defer SetEnabled(prev)

	for _, v := range []bool{true, false} {
		SetEnabled(v)
		for _, got := range []string{Dim(""), Bold(""), Red(""), Green(""), Yellow(""), Cyan("")} {
			if got != "" {
				t.Errorf("enabled=%v: got %q, want empty", v, got)
			}
		}
	}
}
