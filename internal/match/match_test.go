package match

import (
	"encoding/json"
	"os"
	"testing"

	"doppel.moe/katydid/internal/mb"
	"doppel.moe/katydid/internal/mb/mbtest"
)

func loadSearch(t *testing.T, name string) []mb.SearchRelease {
	t.Helper()
	data, err := os.ReadFile("../mb/testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var out struct {
		Releases []mb.SearchRelease `json:"releases"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return out.Releases
}

var drukqsEvidence = Evidence{
	Artist:      "Aphex Twin",
	Album:       "Drukqs",
	Year:        2001,
	TrackCount:  30,
	TrackTitles: []string{"Jynweythek", "Vordhosbn", "Lornaderek", "Nanou2"},
}

func TestRankDrukqsRealFixture(t *testing.T) {
	releases := loadSearch(t, "search-drukqs.json")
	ranked := Rank(drukqsEvidence, releases)
	if len(ranked) == 0 {
		t.Fatalf("rank: got no candidates")
	}

	top := ranked[0]
	if top.Title != "Drukqs" {
		t.Errorf("top title: got %q, want Drukqs", top.Title)
	}
	if top.TrackCount != 30 {
		t.Errorf("top track count: got %d, want 30 (the 2CD original)", top.TrackCount)
	}
	if !top.Auto(drukqsEvidence) {
		t.Errorf("top candidate below auto threshold: score=%.3f title=%.3f count=%.3f year=%.3f",
			top.Score, top.TitleSim, top.TrackCountSim, top.YearSim)
	}
	for i := 1; i < len(ranked); i++ {
		if ranked[i].Score > ranked[i-1].Score {
			t.Errorf("rank not sorted: [%d]=%.3f > [%d]=%.3f", i, ranked[i].Score, i-1, ranked[i-1].Score)
		}
	}
	groups := map[string]bool{}
	for _, candidate := range ranked {
		if candidate.GroupID == "" {
			continue
		}
		if groups[candidate.GroupID] {
			t.Errorf("duplicate group %s in ranked list", candidate.GroupID)
		}
		groups[candidate.GroupID] = true
	}
}

func TestRankGazaAndSaetia(t *testing.T) {
	gaza := Rank(Evidence{Artist: "Gaza", Album: "No Absolutes in Human Suffering", Year: 2012, TrackCount: 11},
		loadSearch(t, "search-gaza.json"))
	if len(gaza) == 0 {
		t.Fatalf("gaza: no candidates")
	}
	if gaza[0].Title != "No Absolutes in Human Suffering" || !gaza[0].Auto(Evidence{Artist: "Gaza", Album: "No Absolutes in Human Suffering", Year: 2012, TrackCount: 11}) {
		t.Errorf("gaza top: %+v", gaza[0])
	}

	saetiaEv := Evidence{Artist: "Saetia", Album: "Saetia", TrackCount: 9}
	saetia := Rank(saetiaEv, loadSearch(t, "search-saetia.json"))
	if len(saetia) == 0 {
		t.Fatalf("saetia: no candidates")
	}
	if saetia[0].Title != "Saetia" {
		t.Errorf("saetia top title: got %q", saetia[0].Title)
	}
}

func TestSimilarity(t *testing.T) {
	cases := []struct {
		a, b  string
		want  float64
		exact bool
	}{
		{"Drukqs", "Drukqs", 1, true},
		{"drukqs!", "Drukqs", 1, true},
		{"No Absolutes in Human Suffering", "No Absolutes In Human Suffering", 1, true},
		{"", "", 1, true},
		{"", "x", 0, true},
		{"completely different", "Drukqs", 0, false},
		{"Drukqs", "Drukqs (Deluxe Edition)", 0.3, false},
		{"Mt Saint Michel", "Mt Saint Michel + Saint Michaels Mount", 0.4, false},
	}
	for _, tc := range cases {
		got := Similarity(tc.a, tc.b)
		if tc.exact && got != tc.want {
			t.Errorf("Similarity(%q, %q): got %f, want %f", tc.a, tc.b, got, tc.want)
		}
		if !tc.exact && (got < tc.want || got > 1) {
			t.Errorf("Similarity(%q, %q): got %f, want in [%.2f, 1]", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSpecifierAuto(t *testing.T) {
	strong := Candidate{Score: 0.95, TitleSim: 1, ArtistSim: 1, YearSim: 1}
	if !SpecifierAuto(strong, 0) {
		t.Errorf("near-exact names should auto")
	}
	if !SpecifierAuto(strong, 1998) {
		t.Errorf("agreeing year should auto")
	}
	weakTitle := Candidate{Score: 1, TitleSim: 0.8, ArtistSim: 1}
	if SpecifierAuto(weakTitle, 0) {
		t.Errorf("title below 0.90 should not auto from specifier alone")
	}
	weakArtist := Candidate{Score: 1, TitleSim: 1, ArtistSim: 0.85}
	if SpecifierAuto(weakArtist, 0) {
		t.Errorf("artist below 0.90 should not auto from specifier alone")
	}
	wrongYear := Candidate{Score: 1, TitleSim: 1, ArtistSim: 1, YearSim: 0.2}
	if SpecifierAuto(wrongYear, 2001) {
		t.Errorf("requested year disagreement should block auto")
	}
	noYear := Candidate{Score: 1, TitleSim: 1, ArtistSim: 1, YearSim: 0.5}
	if !SpecifierAuto(noYear, 0) {
		t.Errorf("no requested year should not block auto")
	}
	zeroCount := Candidate{Score: 0.95, TitleSim: 1, ArtistSim: 1, TrackCountSim: 0.5}
	if !SpecifierAuto(zeroCount, 0) {
		t.Errorf("track count is unknowable from a bare specifier and must not gate")
	}
}

func TestAutoThresholdBoundaries(t *testing.T) {
	ev := Evidence{Artist: "x", Album: "y", Year: 2001, TrackCount: 10}
	strong := Candidate{Score: AutoThreshold, TitleSim: 1, TrackCountSim: 1, YearSim: 1}
	if !strong.Auto(ev) {
		t.Errorf("strong candidate should auto")
	}
	weakScore := Candidate{Score: AutoThreshold - 0.01, TitleSim: 1, TrackCountSim: 1, YearSim: 1}
	if weakScore.Auto(ev) {
		t.Errorf("score below threshold should not auto")
	}
	weakTitle := Candidate{Score: 1, TitleSim: 0.5, TrackCountSim: 1, YearSim: 1}
	if weakTitle.Auto(ev) {
		t.Errorf("weak title similarity should not auto")
	}
	weakCount := Candidate{Score: 1, TitleSim: 1, TrackCountSim: 0.5, YearSim: 1}
	if weakCount.Auto(ev) {
		t.Errorf("weak track count similarity should not auto")
	}
	noYear := Candidate{Score: 1, TitleSim: 1, TrackCountSim: 1, YearSim: 0.5}
	if !noYear.Auto(ev) {
		t.Errorf("missing year (neutral 0.5) should not block auto")
	}
	wrongYear := Candidate{Score: 1, TitleSim: 1, TrackCountSim: 1, YearSim: 0.2}
	if wrongYear.Auto(ev) {
		t.Errorf("wrong year should block auto")
	}
}

func TestTrackCountSimilarity(t *testing.T) {
	if got := trackCountSimilarity(30, 30); got != 1 {
		t.Errorf("30 vs 30: got %f, want 1", got)
	}
	if got := trackCountSimilarity(30, 32); got < 0.93 || got > 0.94 {
		t.Errorf("30 vs 32: got %f, want ~0.9375", got)
	}
	if got := trackCountSimilarity(0, 30); got != 0.5 {
		t.Errorf("missing count: got %f, want 0.5", got)
	}
}

func TestYearSimilarity(t *testing.T) {
	if got := yearSimilarity(2001, 2001); got != 1 {
		t.Errorf("same year: got %f", got)
	}
	if got := yearSimilarity(0, 2001); got != 0.5 {
		t.Errorf("missing year: got %f, want 0.5", got)
	}
	if got := yearSimilarity(2001, 0); got != 0.5 {
		t.Errorf("dateless release with known year: got %f, want 0.5 (unknown is neutral)", got)
	}
	if got := yearSimilarity(2001, 2015); got != 0.2 {
		t.Errorf("far years: got %f, want 0.2", got)
	}
}

func TestCoverage(t *testing.T) {
	release := &mb.Release{Media: []mb.ReleaseMedia{
		{Position: 1, Tracks: []mb.ReleaseTrack{{Position: 1, Title: "Jynweythek"}, {Position: 2, Title: "Vordhosbn"}}},
	}}
	if got := Coverage([]string{"Jynweythek", "Vordhosbn"}, release); got < 0.99 {
		t.Errorf("exact coverage: got %f, want 1", got)
	}
	if got := Coverage([]string{"Totally Other Song"}, release); got > 0.3 {
		t.Errorf("unrelated coverage: got %f, want low", got)
	}
	if got := Coverage(nil, release); got != 0 {
		t.Errorf("no evidence titles: got %f, want 0", got)
	}
}

func TestRankPrefersOldestDate(t *testing.T) {
	releases := []mb.SearchRelease{
		syntheticWithMedia("remaster", "R", "Album", "Artist", "2011-01-01", 10, "CD"),
		syntheticWithMedia("original", "O", "Album", "Artist", "1998", 10, "CD"),
		syntheticWithMedia("dateless", "D", "Album", "Artist", "", 10, "Digital Media"),
	}
	ev := Evidence{Artist: "Artist", Album: "Album", TrackCount: 10}
	for _, release := range releases {
		if c := scoreCandidate(ev, release); c.Score < scoreTieWindow*2 {
			t.Fatalf("test setup: scores must be near-equal for the date tiebreak to apply, got %f", c.Score)
		}
	}
	ranked := Rank(ev, releases)
	if ranked[0].ReleaseID != "original" || ranked[1].ReleaseID != "remaster" || ranked[2].ReleaseID != "dateless" {
		t.Fatalf("order: %s, %s, %s", ranked[0].ReleaseID, ranked[1].ReleaseID, ranked[2].ReleaseID)
	}
}

func TestRankPrefersFormatOnDateTie(t *testing.T) {
	releases := []mb.SearchRelease{
		syntheticWithMedia("tape", "T", "Album", "Artist", "1998", 10, "Cassette"),
		syntheticWithMedia("vinyl", "V", "Album", "Artist", "1998", 10, `12" Vinyl`),
		syntheticWithMedia("cd", "C", "Album", "Artist", "1998", 10, "CD"),
		syntheticWithMedia("digital", "G", "Album", "Artist", "1998", 10, "Digital Media"),
	}
	ranked := Rank(Evidence{Artist: "Artist", Album: "Album", TrackCount: 10}, releases)
	want := []string{"digital", "cd", "vinyl", "tape"}
	for i, id := range want {
		if ranked[i].ReleaseID != id {
			t.Fatalf("position %d: got %s, want %s", i, ranked[i].ReleaseID, id)
		}
	}
}

func TestFormatPriority(t *testing.T) {
	cases := map[string]int{
		"Digital Media": 0, "CD": 1, `12" Vinyl`: 2, "Vinyl": 2,
		"Cassette": 3, "8-Track Cartridge": 4, "": 4, "Mini-Disc": 4,
	}
	for format, want := range cases {
		if got := FormatPriority(format); got != want {
			t.Errorf("FormatPriority(%q) = %d, want %d", format, got, want)
		}
	}
}

func TestDistinctFormats(t *testing.T) {
	media := []mb.ReleaseMedia{
		{Format: "CD"}, {Format: "CD"}, {Format: "DVD-Video"}, {Format: ""},
	}
	formats := DistinctFormats(media)
	if len(formats) != 2 || formats[0] != "CD" || formats[1] != "DVD-Video" {
		t.Fatalf("formats: %+v", formats)
	}
}

func syntheticWithMedia(id, _, title, artist, date string, trackCount int, format string) mb.SearchRelease {
	release := mbtest.SyntheticSearch(id, "group-"+id, title, artist, date, trackCount)
	release.Media = []mb.ReleaseMedia{{Position: 1, Format: format, TrackCount: trackCount}}
	return release
}

func TestRankGroupTypeBreaksTie(t *testing.T) {
	// an album and its single share title, artist, and year; only the
	// primary type separates them
	releases := []mb.SearchRelease{
		syntheticTyped("rg-single", "Single"),
		syntheticTyped("rg-album", "Album"),
	}
	ranked := Rank(Evidence{Artist: "Artist", Album: "Album"}, releases)
	if ranked[0].ReleaseID != "rg-album" {
		t.Fatalf("top group = %s, want the album", ranked[0].ReleaseID)
	}
}

func syntheticTyped(id, primaryType string) mb.SearchRelease {
	release := mbtest.SyntheticSearch(id, id, "Album", "Artist", "1986", 0)
	release.ReleaseGroup = &struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		PrimaryType string `json:"primary-type"`
	}{ID: id, Title: "Album", PrimaryType: primaryType}
	return release
}
