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
		{"basic", "Nirvana - In Utero", []listLine{{1, "Nirvana - In Utero", 0, "Nirvana", "In Utero"}}},
		{"en dash", "A – B", []listLine{{1, "A – B", 0, "A", "B"}}},
		{"em dash", "A — B", []listLine{{1, "A — B", 0, "A", "B"}}},
		{"numbered dot", "01. A - B", []listLine{{1, "01. A - B", 0, "A", "B"}}},
		{"numbered paren", "3) A - B", []listLine{{1, "3) A - B", 0, "A", "B"}}},
		{"skips comment and blank", "# note\n\nA - B", []listLine{{3, "A - B", 0, "A", "B"}}},
		{"no separator", "Just Text", []listLine{{1, "Just Text", 0, "", ""}}},
		{"extra separators", "A - B - C", []listLine{{1, "A - B - C", 0, "A", "B - C"}}},
		{"trims whitespace", "  A  -  B  ", []listLine{{1, "A  -  B", 0, "A", "B"}}},
		{"leading year", "1979 Brian Eno - Ambient 1: Music for Airports", []listLine{
			{1, "1979 Brian Eno - Ambient 1: Music for Airports", 1979, "Brian Eno", "Ambient 1: Music for Airports"},
		}},
		{"year in parens", "(1979) A - B", []listLine{{1, "(1979) A - B", 1979, "A", "B"}}},
		{"year with dash leaves no artist", "1979 - B", []listLine{{1, "1979 - B", 1979, "", ""}}},
		{"year in album stays", "A - 1979", []listLine{{1, "A - 1979", 0, "A", "1979"}}},
		{"order and numbers", "A - B\n#c\n\nD – E\nF", []listLine{
			{1, "A - B", 0, "A", "B"},
			{4, "D – E", 0, "D", "E"},
			{5, "F", 0, "", ""},
		}},
	}
	for _, c := range cases {
		if got := parseAlbumList(c.raw); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: parseAlbumList(%q) = %+v, want %+v", c.name, c.raw, got, c.want)
		}
	}
}
