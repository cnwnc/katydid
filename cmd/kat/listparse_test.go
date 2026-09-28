package main

import (
	"reflect"
	"testing"
)

func TestParseAlbumList(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []listLine
	}{
		{"basic", "Nirvana - In Utero", []listLine{{1, "Nirvana - In Utero", "Nirvana", "In Utero"}}},
		{"en dash", "A – B", []listLine{{1, "A – B", "A", "B"}}},
		{"em dash", "A — B", []listLine{{1, "A — B", "A", "B"}}},
		{"numbered dot", "01. A - B", []listLine{{1, "01. A - B", "A", "B"}}},
		{"numbered paren", "3) A - B", []listLine{{1, "3) A - B", "A", "B"}}},
		{"skips comment and blank", "# note\n\nA - B", []listLine{{3, "A - B", "A", "B"}}},
		{"no separator", "Just Text", []listLine{{1, "Just Text", "", ""}}},
		{"extra separators", "A - B - C", []listLine{{1, "A - B - C", "A", "B - C"}}},
		{"trims whitespace", "  A  -  B  ", []listLine{{1, "A  -  B", "A", "B"}}},
		{"order and numbers", "A - B\n#c\n\nD – E\nF", []listLine{
			{1, "A - B", "A", "B"},
			{4, "D – E", "D", "E"},
			{5, "F", "", ""},
		}},
	}
	for _, c := range cases {
		if got := parseAlbumList(c.raw); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: parseAlbumList(%q) = %+v, want %+v", c.name, c.raw, got, c.want)
		}
	}
}
