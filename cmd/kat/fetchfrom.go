package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"doppel.moe/katydid/internal/ansi"
	"doppel.moe/katydid/internal/api"
	"doppel.moe/katydid/internal/cli"
	"doppel.moe/katydid/internal/match"
)

// fetchdSocket is the want-queue socket; kat's main client talks to
// katyd, the queue lives on fetchd.
func fetchdSocket() string {
	if socket := os.Getenv("KATYFETCHD_SOCKET"); socket != "" {
		return socket
	}
	return "/run/katy-fetchd/fetchd.sock"
}

// splitName matches the album side of an LLM-generated split line:
// lists write "A / B - Split" where MusicBrainz stores the split under
// the joined artist name as both artist and title.
var splitName = regexp.MustCompile(`(?i)\bsplit\b`)

func runFetchFrom(katyd *cli.Client, args []string) error {
	flags := flag.NewFlagSet("fetchfrom", flag.ContinueOnError)
	dryRun := flags.Bool("dry-run", false, "print decisions without queueing anything")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: kat fetchfrom [-dry-run] <list.txt>")
	}
	path := flags.Arg(0)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read list: %w", err)
	}
	fetchd := cli.Dial(fetchdSocket())

	queued, err := fetchd.Wants()
	if err != nil {
		return fmt.Errorf("list wants: %w", err)
	}
	wanted := map[string]string{}
	for _, want := range queued {
		wanted[wantKey(want.Artist, want.Album)] = want.ID
	}

	lines := parseAlbumList(string(raw))
	if len(lines) == 0 {
		return errors.New("list has no entries")
	}
	batch := &batchFetcher{katyd: katyd, fetchd: fetchd, wanted: wanted, dryRun: *dryRun}
	for _, line := range lines {
		batch.line(line)
	}
	fmt.Fprintf(os.Stderr, "fetchfrom: %d queued, %d skipped, %d rejected of %d line(s)\n",
		batch.queued, batch.skipped, batch.rejected, len(lines))
	if batch.rejected > 0 {
		rejectsPath := strings.TrimSuffix(path, ".txt") + ".rejects"
		if err := os.WriteFile(rejectsPath, []byte(strings.Join(batch.rejects, "\n")+"\n"), 0o644); err != nil {
			return fmt.Errorf("write rejects: %w", err)
		}
		fmt.Fprintf(os.Stderr, "fetchfrom: rejects written to %s\n", rejectsPath)
	}
	return nil
}

func wantKey(artist, album string) string {
	return strings.ToLower(artist) + "\x00" + strings.ToLower(album)
}

type batchFetcher struct {
	katyd    *cli.Client
	fetchd   *cli.Client
	wanted   map[string]string
	dryRun   bool
	queued   int
	skipped  int
	rejected int
	rejects  []string
}

// line resolves and queues one list entry; a bad entry is rejected and
// the batch continues.
func (b *batchFetcher) line(line listLine) {
	if line.Artist == "" {
		b.reject(line, "cannot parse, no \" - \" separator")
		return
	}
	if _, ok := b.wanted[wantKey(line.Artist, line.Album)]; ok {
		b.skipped++
		fmt.Printf("%s %s\n", ansi.Yellow("skip"), ansi.Dim(line.Raw+" already queued"))
		return
	}
	names, decision, err := b.resolveLine(line.Artist, line.Album)
	if err != nil {
		b.reject(line, err.Error())
		return
	}
	fmt.Printf("%s %s -> %s (%s)\n", ansi.Green("queue"), line.Raw,
		names.Artist+" - "+names.Title, ansi.Dim(decision))
	b.queued++
	if b.dryRun {
		return
	}
	spec := cli.WantSpec{
		Artist:        line.Artist,
		Album:         line.Album,
		Group:         names.GroupID,
		ReleaseArtist: names.Artist,
		ReleaseTitle:  names.Title,
	}
	if _, err := b.fetchd.AddWant(spec); err != nil {
		b.queued--
		b.reject(line, "queue want: "+err.Error())
		return
	}
}

func (b *batchFetcher) reject(line listLine, reason string) {
	b.rejected++
	b.rejects = append(b.rejects, fmt.Sprintf("%s # %s", line.Raw, reason))
	fmt.Printf("%s %s: %s\n", ansi.Red("reject"), line.Raw, ansi.Dim(reason))
}

// names is the resolved identity a want is pinned to.
type names struct {
	GroupID string
	Artist  string
	Title   string
}

// maxResolveSteps bounds the split-retry and alias loops per line.
const maxResolveSteps = 4

// resolveLine walks the ladder: plain resolve, split-name retry
// (album := artist, MB stores splits under the joined artist), then
// model respellings. Every accepted identity comes from MusicBrainz
// candidates; the model only chooses or respells.
func (b *batchFetcher) resolveLine(artist, album string) (names, string, error) {
	original := names{Artist: artist, Title: album}
	current := original
	aliased := false
	for step := 0; step < maxResolveSteps; step++ {
		response, err := b.katyd.ResolveLimit(current.Artist, current.Title, 0, "", 8)
		if err != nil {
			return names{}, "", fmt.Errorf("resolve %s - %s: %w", current.Artist, current.Title, err)
		}
		if len(response.Candidates) > 0 {
			picked, how, err := b.pickCandidate(current.Artist+" - "+current.Title, response)
			if err != nil {
				return names{}, "", err
			}
			if aliased {
				how = "alias " + how
			}
			return picked, how, nil
		}
		// zero candidates: the split heuristic runs once, before the
		// model recoverer, and only on an untried split-shaped album
		if !aliased && splitName.MatchString(current.Title) {
			retry := names{Artist: current.Artist, Title: current.Artist}
			alt, err := b.katyd.ResolveLimit(retry.Artist, retry.Title, 0, "", 8)
			if err == nil && len(alt.Candidates) > 0 {
				// the resolved name is the actual name: restart the
				// loop with it
				current = names{Artist: alt.Candidates[0].Artist, Title: alt.Candidates[0].Title}
				continue
			}
		}
		// model recoverer: respellings of the names that failed
		aliases, err := b.katyd.LLMAlias(original.Artist, original.Title)
		if err != nil {
			return names{}, "", fmt.Errorf("no candidates for %s - %s (%s)", original.Artist, original.Title, err)
		}
		if len(aliases.Variants) == 0 {
			return names{}, "", fmt.Errorf("no candidates for %s - %s and no respellings", original.Artist, original.Title)
		}
		aliased = true
		for _, variant := range aliases.Variants {
			response, err := b.katyd.ResolveLimit(variant.Artist, variant.Album, 0, "", 8)
			if err != nil || len(response.Candidates) == 0 {
				continue
			}
			picked, how, err := b.pickCandidate(variant.Artist+" - "+variant.Album, response)
			if err != nil {
				return names{}, "", err
			}
			return picked, "alias " + variant.Artist + " - " + variant.Album + " " + how, nil
		}
		return names{}, "", fmt.Errorf("no candidates for %s - %s after %d respelling(s)", original.Artist, original.Title, len(aliases.Variants))
	}
	return names{}, "", fmt.Errorf("no candidates for %s - %s after %d steps", original.Artist, original.Title, maxResolveSteps)
}

// pickCandidate turns a candidate response into a pinned identity:
// auto responses take the top candidate, ambiguous ones go to the
// model, which may only return an id it was offered.
func (b *batchFetcher) pickCandidate(request string, response api.ResolveResponse) (names, string, error) {
	if response.Auto {
		top := response.Candidates[0]
		return names{GroupID: top.GroupID, Artist: top.Artist, Title: top.Title}, "auto", nil
	}
	options := make([]cli.LLMPickOption, 0, len(response.Candidates))
	for _, candidate := range response.Candidates {
		options = append(options, cli.LLMPickOption{
			ID:     candidateID(candidate),
			Title:  candidate.Title,
			Artist: candidate.Artist,
			Date:   candidate.Date,
			Tracks: candidate.TrackCount,
			Type:   candidate.PrimaryType,
		})
	}
	reply, err := b.katyd.LLMPick(request, options)
	if err != nil {
		return names{}, "", fmt.Errorf("ambiguous, %d candidates (%s)", len(response.Candidates), err)
	}
	if reply.Pick == "" {
		note := reply.Note
		if note == "" {
			note = "no reason given"
		}
		return names{}, "", fmt.Errorf("ambiguous, model picked none of %d candidates (%s)", len(response.Candidates), note)
	}
	for _, candidate := range response.Candidates {
		if candidateID(candidate) == reply.Pick {
			return names{GroupID: candidate.GroupID, Artist: candidate.Artist, Title: candidate.Title}, "llm: " + reply.Note, nil
		}
	}
	return names{}, "", fmt.Errorf("model returned an id outside the offered candidates")
}

// candidateID prefers the group id: wants pin groups and let the
// fetcher pick the oldest release inside.
func candidateID(candidate match.Candidate) string {
	if candidate.GroupID != "" {
		return candidate.GroupID
	}
	return candidate.ReleaseID
}
