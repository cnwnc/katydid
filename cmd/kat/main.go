package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"doppel.moe/katydid/internal/cli"
	"doppel.moe/katydid/internal/importer"
	"doppel.moe/katydid/internal/library"
)

const usage = `kat: query the katydid library daemon

usage:
  kat status
  kat scan
  kat list [query] [-artist=] [-year=] [-json] [-format=]
  kat show <album-id> [-json]
  kat check [-json]
  kat import <dir> [-artist=] [-album=] [-year=] [-mbid=] [-pick=N] [-skip] [-replace]
              [-by=] [-request="..."]
  kat decide <token> <N|Y|done|skip>, or <token> remap <file> <track>
  kat retag [-all | <album-id>] [-policy=]
  kat decisions

environment:
  KATYDID_SOCKET  unix socket path (default /run/katyd/katyd.sock)`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	client := Dial()
	switch os.Args[1] {
	case "status":
		err = runStatus(client)
	case "scan":
		err = runScan(client)
	case "list":
		err = runList(client, os.Args[2:])
	case "show":
		err = runShow(client, os.Args[2:])
	case "check":
		err = runCheck(client, os.Args[2:])
	case "import":
		err = runImport(client, os.Args[2:])
	case "decide":
		err = runDecide(client, os.Args[2:])
	case "retag":
		err = runRetag(client, os.Args[2:])
	case "decisions":
		err = runDecisions(client, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "kat: unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "kat: %v\n", err)
		os.Exit(1)
	}
}

func Dial() *cli.Client {
	socket := os.Getenv("KATYDID_SOCKET")
	if socket == "" {
		socket = "/run/katyd/katyd.sock"
	}
	return cli.Dial(socket)
}

func runStatus(client *cli.Client) error {
	status, err := client.Status()
	if err != nil {
		return err
	}
	s := status.Status
	fmt.Printf("katyd %s\nlibrary %s\nalbums %d (%d pending), %d files\nscanned %s (%.2fs)\n",
		s.Version, s.Library, s.Albums, s.Pending, s.Files, s.ScannedAt.Format("2006-01-02 15:04:05"), s.ScanDuration)
	return nil
}

func runScan(client *cli.Client) error {
	scan, err := client.Scan()
	if err != nil {
		return err
	}
	s := scan.Status
	fmt.Printf("scanned %d albums (%d pending), %d files in %.2fs\n", s.Albums, s.Pending, s.Files, s.ScanDuration)
	return nil
}

const defaultListFormat = "{year} {artist} - {album}"

var formatFieldRe = regexp.MustCompile(`\{([a-z]+)\}`)

func renderAlbumFormat(template string, album library.Album) (string, error) {
	year := ""
	if album.Meta.Year != 0 {
		year = strconv.Itoa(album.Meta.Year)
	}
	fields := map[string]string{
		"album":  album.Meta.Album,
		"artist": album.Meta.AlbumArtist,
		"path":   album.ID,
		"tracks": strconv.Itoa(len(album.Meta.Tracks)),
		"year":   year,
	}
	for _, match := range formatFieldRe.FindAllStringSubmatch(template, -1) {
		if _, ok := fields[match[1]]; !ok {
			return "", fmt.Errorf("unknown format field %q; fields are album, artist, path, tracks, year", match[1])
		}
	}
	return strings.TrimSpace(formatFieldRe.ReplaceAllStringFunc(template, func(match string) string {
		return fields[match[1:len(match)-1]]
	})), nil
}

func runList(client *cli.Client, args []string) error {
	flags := flag.NewFlagSet("list", flag.ExitOnError)
	artist := flags.String("artist", "", "filter by albumartist substring")
	year := flags.Int("year", 0, "filter by exact year")
	asJSON := flags.Bool("json", false, "output raw json")
	format := flags.String("format", "", "line format, e.g. \""+defaultListFormat+"\"")
	positional, rest := splitFlags(args, map[string]bool{"json": true, "format": true})
	if err := flags.Parse(rest); err != nil {
		return err
	}
	if *asJSON && *format != "" {
		return errors.New("use -json or -format, not both")
	}
	query := library.Query{Q: strings.Join(positional, " "), Artist: *artist, Year: *year}

	albums, err := client.Albums(query)
	if err != nil {
		return err
	}
	if len(albums.Albums) == 0 {
		return errors.New("library is empty")
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(albums)
	}
	template := *format
	if template == "" {
		template = defaultListFormat
	}
	for _, album := range albums.Albums {
		line, err := renderAlbumFormat(template, album)
		if err != nil {
			return err
		}
		fmt.Println(line + pendingMark(album))
	}
	return nil
}

func runShow(client *cli.Client, args []string) error {
	flags := flag.NewFlagSet("show", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "output raw json")
	positional, rest := splitFlags(args, map[string]bool{"json": true})
	if err := flags.Parse(rest); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: kat show <album-id>")
	}
	album, err := client.Album(positional[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(album)
	}
	fmt.Printf("%s (%d) — %s%s\n", album.Meta.AlbumArtist, album.Meta.Year, album.Meta.Album, pendingMark(album))
	fmt.Printf("id %s, %d files\n", album.ID, len(album.Files))
	for _, track := range album.Meta.Tracks {
		fmt.Printf("  %2d. %s (%s)\n", track.Track, track.Title, track.File)
	}
	return nil
}

func runCheck(client *cli.Client, args []string) error {
	flags := flag.NewFlagSet("check", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "output raw json")
	_, rest := splitFlags(args, map[string]bool{"json": true})
	if err := flags.Parse(rest); err != nil {
		return err
	}
	check, err := client.Check()
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(check)
	}
	if len(check.Findings) == 0 {
		fmt.Println("no findings")
		return nil
	}
	for _, finding := range check.Findings {
		line := fmt.Sprintf("%s  %s", finding.Album, finding.Kind)
		if finding.File != "" {
			line += "  " + finding.File
		}
		fmt.Println(line)
	}
	return nil
}

func pendingMark(album library.Album) string {
	if album.Pending {
		return " [pending]"
	}
	return ""
}

func runImport(client *cli.Client, args []string) error {
	flags := flag.NewFlagSet("import", flag.ExitOnError)
	artist := flags.String("artist", "", "artist hint, overrides file tags")
	album := flags.String("album", "", "album hint, overrides file tags")
	year := flags.Int("year", 0, "year hint, overrides file tags")
	mbid := flags.String("mbid", "", "musicbrainz release id, skips matching entirely")
	pick := flags.Int("pick", 0, "pick candidate N instead of prompting")
	skip := flags.Bool("skip", false, "skip a pending decision instead of prompting")
	replace := flags.Bool("replace", false, "replace an existing album at the target path")
	by := flags.String("by", "", "who requested this import")
	request := flags.String("request", "", "original request text")
	positional, rest := splitFlags(args, map[string]bool{"skip": true, "replace": true})
	if err := flags.Parse(rest); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: kat import <dir>")
	}
	abs, err := filepath.Abs(positional[0])
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}

	result, err := client.Import(importer.Request{
		Dir: abs, Artist: *artist, Album: *album, Year: *year, MBID: *mbid,
		Replace: *replace, By: *by, Request: *request,
	})
	if err != nil {
		return err
	}
	return handleImportResult(client, result, *pick, *skip)
}

func handleImportResult(client *cli.Client, result importer.Result, pick int, skip bool) error {
	switch result.Status {
	case "imported":
		if result.Format != "" {
			fmt.Printf("imported: %s (%s)\n", result.AlbumID, result.Format)
		} else {
			fmt.Printf("imported: %s\n", result.AlbumID)
		}
		for _, note := range result.Notes {
			fmt.Printf("note: %s\n", note)
		}
		return nil
	case "skipped":
		fmt.Println("skipped")
		return nil
	}

	decision := result.Decision
	ev := decision.Evidence
	for _, note := range result.Notes {
		fmt.Printf("note: %s\n", note)
	}
	fmt.Printf("low confidence for %s - %s (%d files, %d)\n", ev.Artist, ev.Album, ev.TrackCount, ev.Year)
	fmt.Printf("decision: %s\n", decision.Token)
	fmt.Println("candidates:")
	for _, candidate := range decision.Candidates {
		parts := []string{}
		if candidate.Date != "" {
			parts = append(parts, candidate.Date)
		}
		if len(candidate.Formats) > 0 {
			parts = append(parts, strings.Join(candidate.Formats, "+"))
		}
		parts = append(parts, fmt.Sprintf("%d tracks", candidate.TrackCount))
		marker := " "
		if candidate.Pairable {
			marker = "*"
		}
		fmt.Printf("  %s%d. [%0.2f] %s - %s (%s)\n",
			marker, candidate.Number, candidate.Score, candidate.Artist, candidate.Title, strings.Join(parts, ", "))
	}
	if decision.Remap != nil {
		fmt.Println("(*) would accept these files unchanged")
	}

	if pick != 0 {
		return decideSend(client, decision.Token, importer.DecideInput{Pick: pick})
	}
	if skip {
		return decideSend(client, decision.Token, importer.DecideInput{Skip: true})
	}
	if !isTerminal(os.Stdin) {
		fmt.Printf("decision required: kat decide %s <N|Y|done|skip>, or \"kat decide %s remap <file> <track>\" to reassign\n", decision.Token, decision.Token)
		return nil
	}
	return decideInteractive(client, result)
}

// decideInteractive drives the decide/remap conversation: candidate
// choice, then the file-to-track table when the pick does not fit.
func decideInteractive(client *cli.Client, result importer.Result) error {
	token := result.Decision.Token
	for {
		if result.Status == "imported" || result.Status == "skipped" {
			return reportResult(result)
		}
		decision := result.Decision
		if decision.Remap == nil {
			choice, err := promptChoice(len(decision.Candidates))
			if err != nil {
				return err
			}
			if choice == "skip" {
				return decideSend(client, token, importer.DecideInput{Skip: true})
			}
			if choice == "y" || choice == "Y" {
				result, err = client.Decide(token, importer.DecideInput{Pick: 1})
			} else {
				chosen, convErr := strconv.Atoi(choice)
				if convErr != nil {
					fmt.Printf("%q is not a number or skip\n", choice)
					continue
				}
				result, err = client.Decide(token, importer.DecideInput{Pick: chosen})
			}
			if err != nil {
				return err
			}
			printDecisionState(result)
			continue
		}
		printRemapTable(decision.Remap)
		action, err := promptRemap(decision.Remap)
		if err != nil {
			return err
		}
		switch action.kind {
		case remapPair:
			result, err = client.Decide(token, importer.DecideInput{RemapFile: action.file, RemapTrack: action.track})
		case remapAccept:
			result, err = client.Decide(token, importer.DecideInput{Accept: true})
		case remapSkip:
			return decideSend(client, token, importer.DecideInput{Skip: true})
		}
		if err != nil {
			return err
		}
		printDecisionState(result)
	}
}

func promptChoice(count int) (string, error) {
	fmt.Printf("use which? [1-%d/skip]: ", count)
	line := ""
	if _, err := fmt.Scanln(&line); err != nil {
		return "", fmt.Errorf("read choice: %w", err)
	}
	return line, nil
}

func printRemapTable(table *importer.Remap) {
	fmt.Printf("pairing for candidate %d (files 1-%d, tracks 1-%d):\n", table.Pick, len(table.Files), len(table.Tracks))
	for _, file := range table.Files {
		if trackIndex, ok := table.Map[file.Index]; ok {
			track := table.Tracks[trackIndex-1]
			fmt.Printf("  %02d: %q -> %s %s\n", file.Index, file.File, track.Number, track.Title)
		} else {
			fmt.Printf("  %02d: %q\n", file.Index, file.File)
		}
	}
}

const (
	remapPair   = "pair"
	remapAccept = "accept"
	remapSkip   = "skip"
)

type remapAction struct {
	kind  string
	file  int
	track int
}

func promptRemap(table *importer.Remap) (remapAction, error) {
	for {
		fmt.Printf("file index (done/skip): ")
		line := ""
		if _, err := fmt.Scanln(&line); err != nil {
			return remapAction{}, fmt.Errorf("read file index: %w", err)
		}
		switch line {
		case "done":
			return remapAction{kind: remapAccept}, nil
		case "skip":
			return remapAction{kind: remapSkip}, nil
		}
		file, err := strconv.Atoi(line)
		if err != nil || file < 1 || file > len(table.Files) {
			fmt.Printf("file index must be 1-%d\n", len(table.Files))
			continue
		}
		fmt.Printf("track index: ")
		trackLine := ""
		if _, err := fmt.Scanln(&trackLine); err != nil {
			return remapAction{}, fmt.Errorf("read track index: %w", err)
		}
		track, err := strconv.Atoi(trackLine)
		if err != nil || track < 1 || track > len(table.Tracks) {
			fmt.Printf("track index must be 1-%d\n", len(table.Tracks))
			continue
		}
		return remapAction{kind: remapPair, file: file, track: track}, nil
	}
}

func printDecisionState(result importer.Result) {
	for _, note := range result.Notes {
		fmt.Printf("note: %s\n", note)
	}
}

func reportResult(result importer.Result) error {
	switch result.Status {
	case "imported":
		if result.Format != "" {
			fmt.Printf("imported: %s (%s)\n", result.AlbumID, result.Format)
		} else {
			fmt.Printf("imported: %s\n", result.AlbumID)
		}
		for _, note := range result.Notes {
			fmt.Printf("note: %s\n", note)
		}
	case "skipped":
		fmt.Println("skipped")
	}
	return nil
}

func decideSend(client *cli.Client, token string, in importer.DecideInput) error {
	result, err := client.Decide(token, in)
	if err != nil {
		return err
	}
	if result.Decision != nil && result.Decision.Remap != nil {
		return decideInteractive(client, result)
	}
	return reportResult(result)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func runDecide(client *cli.Client, args []string) error {
	if len(args) == 4 && args[1] == "remap" {
		file, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("file index %q is not a number", args[2])
		}
		track, err := strconv.Atoi(args[3])
		if err != nil {
			return fmt.Errorf("track index %q is not a number", args[3])
		}
		return decideSend(client, args[0], importer.DecideInput{RemapFile: file, RemapTrack: track})
	}
	if len(args) != 2 {
		return errors.New("usage: kat decide <token> <N|Y|done|skip>, or <token> remap <file> <track>")
	}
	token := args[0]
	switch args[1] {
	case "skip":
		return decideSend(client, token, importer.DecideInput{Skip: true})
	case "done":
		return decideSend(client, token, importer.DecideInput{Accept: true})
	case "y", "Y":
		return decideSend(client, token, importer.DecideInput{Pick: 1})
	}
	chosen, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("pick %q is not a number, done, or skip", args[1])
	}
	return decideSend(client, token, importer.DecideInput{Pick: chosen})
}

func runRetag(client *cli.Client, args []string) error {
	flags := flag.NewFlagSet("retag", flag.ExitOnError)
	all := flags.Bool("all", false, "retag every sidecar-backed album")
	policyName := flags.String("policy", "", "policy name (default: library default)")
	positional, rest := splitFlags(args, map[string]bool{"all": true})
	if err := flags.Parse(rest); err != nil {
		return err
	}
	if !*all && len(positional) != 1 {
		return errors.New("usage: kat retag [-all | <album-id>] [-policy=]")
	}
	if *all && len(positional) > 0 {
		return errors.New("usage: kat retag takes an album id or -all, not both")
	}

	album := ""
	if len(positional) == 1 {
		album = positional[0]
	}
	response, err := client.Retag(album, *all, *policyName)
	if err != nil {
		return err
	}
	for _, result := range response.Results {
		if result.Error != "" {
			fmt.Printf("%s: error: %s\n", result.AlbumID, result.Error)
			continue
		}
		fmt.Printf("%s: retagged\n", result.AlbumID)
		for _, note := range result.Notes {
			fmt.Printf("  note: %s\n", note)
		}
	}
	return nil
}

func splitFlags(args []string, boolean map[string]bool) (positional, flagArgs []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		name := strings.TrimLeft(arg, "-")
		if !boolean[name] && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return positional, flagArgs
}

func runDecisions(client *cli.Client, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: kat decisions")
	}
	decisions, err := client.Decisions()
	if err != nil {
		return err
	}
	if len(decisions) == 0 {
		fmt.Println("no pending decisions")
		return nil
	}
	for _, decision := range decisions {
		fmt.Printf("%s  %s - %s (%d files, %d candidates)\n",
			decision.Token, decision.Evidence.Artist, decision.Evidence.Album,
			decision.Evidence.TrackCount, len(decision.Candidates))
	}
	return nil
}
