package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"doppel.moe/katydid/internal/cli"
	"doppel.moe/katydid/internal/library"
)

const usage = `kat: query the katydid library daemon

usage:
  kat status
  kat scan
  kat list [query] [--artist=] [--year=] [--json]
  kat show <album-id> [--json]
  kat check [--json]

environment:
  KATYDID_SOCKET  unix socket path (default /tmp/katyd.sock)`

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
		socket = filepath.Join(os.TempDir(), "katyd.sock")
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

func runList(client *cli.Client, args []string) error {
	flags := flag.NewFlagSet("list", flag.ExitOnError)
	artist := flags.String("artist", "", "filter by albumartist substring")
	year := flags.Int("year", 0, "filter by exact year")
	asJSON := flags.Bool("json", false, "output raw json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	query := library.Query{Q: strings.Join(flags.Args(), " "), Artist: *artist, Year: *year}

	albums, err := client.Albums(query)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(albums)
	}
	for _, album := range albums.Albums {
		state := ""
		if album.Pending {
			state = " [pending]"
		}
		fmt.Printf("%s  %d  %s — %s (%d tracks)%s\n",
			album.ID, album.Meta.Year, album.Meta.AlbumArtist, album.Meta.Album, len(album.Meta.Tracks), state)
	}
	return nil
}

func runShow(client *cli.Client, args []string) error {
	flags := flag.NewFlagSet("show", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "output raw json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: kat show <album-id>")
	}
	album, err := client.Album(flags.Arg(0))
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
	if err := flags.Parse(args); err != nil {
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
