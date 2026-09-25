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
  kat decide <token> <N|Y|skip>
  kat retag [-all | <album-id>] [-policy=]

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
	case "help", "-h", "--help":
		fmt.Println(usage)
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
		fmt.Printf("imported: %s\n", result.AlbumID)
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
	fmt.Printf("low confidence for %s - %s (%d files, %d)\n", ev.Artist, ev.Album, ev.TrackCount, ev.Year)
	fmt.Println("candidates:")
	for _, candidate := range decision.Candidates {
		fmt.Printf("  %d. [%0.2f] %s - %s (%s, %d tracks)\n",
			candidate.Number, candidate.Score, candidate.Artist, candidate.Title, candidate.Date, candidate.TrackCount)
	}

	if pick != 0 {
		return decideAndReport(client, decision.Token, pick, false)
	}
	if skip {
		return decideAndReport(client, decision.Token, 0, true)
	}
	if !isTerminal(os.Stdin) {
		fmt.Printf("decision required: kat decide %s <N|skip>\n", decision.Token)
		return nil
	}
	fmt.Printf("use which? [1-%d/skip]: ", len(decision.Candidates))
	line := ""
	if _, err := fmt.Scanln(&line); err != nil {
		return fmt.Errorf("read choice: %w", err)
	}
	if line == "s" || line == "skip" {
		return decideAndReport(client, decision.Token, 0, true)
	}
	chosen, err := strconv.Atoi(line)
	if err != nil {
		return fmt.Errorf("choice %q is not a number or skip", line)
	}
	return decideAndReport(client, decision.Token, chosen, false)
}

func decideAndReport(client *cli.Client, token string, pick int, skip bool) error {
	result, err := client.Decide(token, pick, skip)
	if err != nil {
		return err
	}
	switch result.Status {
	case "imported":
		fmt.Printf("imported: %s\n", result.AlbumID)
		for _, note := range result.Notes {
			fmt.Printf("note: %s\n", note)
		}
	case "skipped":
		fmt.Println("skipped")
	default:
		fmt.Printf("status: %s\n", result.Status)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func runDecide(client *cli.Client, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: kat decide <token> <N|Y|skip>")
	}
	token := args[0]
	if args[1] == "skip" {
		return decideAndReport(client, token, 0, true)
	}
	if args[1] == "y" || args[1] == "Y" {
		return decideAndReport(client, token, 1, false)
	}
	chosen, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("pick %q is not a number or skip", args[1])
	}
	return decideAndReport(client, token, chosen, false)
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
