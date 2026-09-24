package safe

import (
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		fallback string
		want     string
	}{
		{"plain passes", "Drukqs", "x", "Drukqs"},
		{"slash replaced", "AC/DC", "x", "AC-DC"},
		{"slash album", "Band / Band / Band", "x", "Band - Band - Band"},
		{"nul replaced", "a\x00b", "x", "a-b"},
		{"control chars stripped", "a\x07b\x1b[31m", "x", "ab[31m"},
		{"del stripped", "a\x7fb", "x", "ab"},
		{"leading dots stripped", "...And Justice for All", "x", "And Justice for All"},
		{"trailing dots and spaces trimmed", "Trout Mask Replica . ", "x", "Trout Mask Replica"},
		{"dot only falls back", "...", "Fallback", "Fallback"},
		{"dot dot falls back", "..", "Fallback", "Fallback"},
		{"empty falls back", "   ", "Fallback", "Fallback"},
		{"fallback itself sanitized", "", "AC/DC", "AC-DC"},
		{"cjk unchanged", "きくおミク6", "x", "きくおミク6"},
		{"cyrillic unchanged", "Кино", "x", "Кино"},
		{"cjk with slash", "君/の/名/は", "x", "君-の-名-は"},
		{"nfd normalized to nfc", "cafe\u0301", "x", "café"},
		{"emoji unchanged", "Nirvana (▮-▮)", "x", "Nirvana (▮-▮)"},
	}
	for _, tc := range cases {
		got := Name(tc.input, tc.fallback)
		if got != tc.want {
			t.Errorf("%s: Name(%q) = %q, want %q", tc.name, tc.input, got, tc.want)
		}
	}
}

func TestNameNFDBytes(t *testing.T) {
	if got := Name("cafe\u0301", "x"); got == "cafe\u0301" {
		t.Errorf("NFD input kept decomposed form: %q", got)
	}
}

func TestNameByteCap(t *testing.T) {
	long := strings.Repeat("き", 150) // 450 bytes
	got := Name(long, "x")
	if len(got) > MaxComponentBytes {
		t.Errorf("component is %d bytes, want <= %d", len(got), MaxComponentBytes)
	}
	if !strings.HasPrefix(got, strings.Repeat("き", 10)) {
		t.Errorf("cap did not preserve the prefix: %q", got)
	}
	if strings.HasSuffix(got, "-") || strings.HasSuffix(got, " ") {
		t.Errorf("cap left a dangling separator: %q", got)
	}
}
