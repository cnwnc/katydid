package sidecar

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

const Schema = 1

const FileName = "album.yaml"

var ErrNoSidecar = errors.New("no sidecar")

type Track struct {
	File           string              `yaml:"file"`
	Title          string              `yaml:"title"`
	Track          int                 `yaml:"track"`
	Disc           int                 `yaml:"disc,omitempty"`
	LengthSeconds  float64             `yaml:"length_seconds,omitempty"`
	Artist         string              `yaml:"artist,omitempty"`
	ArtistSort     string              `yaml:"artist_sort,omitempty"`
	Artists        []string            `yaml:"artists,omitempty"`
	RecordingID    string              `yaml:"recording_id,omitempty"`
	ReleaseTrackID string              `yaml:"release_track_id,omitempty"`
	Tags           map[string][]string `yaml:"tags,omitempty"`
}

type MusicBrainz struct {
	ReleaseGroupID string `yaml:"releasegroup,omitempty"`
	ReleaseID      string `yaml:"release,omitempty"`
	Date           string `yaml:"date,omitempty"`
}

// LastFM holds the last.fm provenance for albums imported without a
// musicbrainz entry.
type LastFM struct {
	URL string `yaml:"url,omitempty"`
}

type Origin struct {
	Path string `yaml:"path,omitempty"`
}

type Provenance struct {
	Imported time.Time `yaml:"imported"`
	By       string    `yaml:"by"`
	Request  string    `yaml:"request,omitempty"`
	Origin   Origin    `yaml:"origin,omitempty"`
}

type TagState struct {
	Policy    string    `yaml:"policy"`
	Applied   time.Time `yaml:"applied"`
	StateHash string    `yaml:"state-hash"`
}

type Album struct {
	Schema          int         `yaml:"schema"`
	Album           string      `yaml:"album"`
	AlbumArtist     string      `yaml:"albumartist"`
	AlbumArtists    []string    `yaml:"albumartists,omitempty"`
	AlbumArtistSort string      `yaml:"albumartistsort,omitempty"`
	Year            int         `yaml:"year,omitempty"`
	OriginalDate    string      `yaml:"originaldate,omitempty"`
	Genres          []string    `yaml:"genres,omitempty"`
	ReleaseType     string      `yaml:"releasetype,omitempty"`
	Compilation     bool        `yaml:"compilation,omitempty"`
	Label           string      `yaml:"label,omitempty"`
	CatalogNumber   string      `yaml:"catalognumber,omitempty"`
	MusicBrainz     MusicBrainz `yaml:"musicbrainz"`
	Source          string      `yaml:"source,omitempty"`
	LastFM          LastFM      `yaml:"lastfm,omitempty"`
	Provenance      Provenance  `yaml:"provenance"`
	TagState        *TagState   `yaml:"tags,omitempty"`
	Tracks          []Track     `yaml:"tracks"`
}

func Load(dir string) (*Album, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoSidecar
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var album Album
	if err := yaml.Unmarshal(data, &album); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if album.Schema > Schema {
		return nil, fmt.Errorf("parse %s: sidecar schema %d is newer than supported schema %d", path, album.Schema, Schema)
	}
	if err := album.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &album, nil
}

func Save(dir string, album *Album) error {
	if err := album.Validate(); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	album.Schema = Schema

	data, err := yaml.Marshal(album)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	path := filepath.Join(dir, FileName)
	tmp := filepath.Join(dir, FileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmp, path, err)
	}
	return nil
}

func (a *Album) Validate() error {
	if a.Album == "" {
		return errors.New("album name is empty")
	}
	if a.AlbumArtist == "" {
		return errors.New("albumartist is empty")
	}
	if len(a.Tracks) == 0 {
		return errors.New("tracks list is empty")
	}
	seen := map[string]bool{}
	for _, track := range a.Tracks {
		if track.File == "" {
			return errors.New("track with empty file name")
		}
		if track.Title == "" {
			return fmt.Errorf("track %s has empty title", track.File)
		}
		if seen[track.File] {
			return fmt.Errorf("duplicate track file %s", track.File)
		}
		seen[track.File] = true
	}
	return nil
}
