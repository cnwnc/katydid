package library

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

const Version = "0.1.0"

type TrackMeta struct {
	File          string  `json:"file"`
	Title         string  `json:"title"`
	Track         int     `json:"track,omitempty"`
	Disc          int     `json:"disc,omitempty"`
	LengthSeconds float64 `json:"length_seconds,omitempty"`
}

type Meta struct {
	AlbumArtist string      `json:"albumartist"`
	Album       string      `json:"album"`
	Year        int         `json:"year,omitempty"`
	Tracks      []TrackMeta `json:"tracks"`
}

type Album struct {
	ID      string   `json:"id"`
	Pending bool     `json:"pending"`
	Meta    Meta     `json:"meta"`
	Files   []string `json:"files"`
}

type Query struct {
	Q      string
	Artist string
	Year   int
}

type Status struct {
	Version      string    `json:"version"`
	Library      string    `json:"library"`
	Albums       int       `json:"albums"`
	Pending      int       `json:"pending"`
	Files        int       `json:"files"`
	ScannedAt    time.Time `json:"scanned_at"`
	ScanDuration float64   `json:"scan_seconds"`
}

type Index struct {
	root string

	mu          sync.RWMutex
	albums      []Album
	byID        map[string]*Album
	scannedAt   time.Time
	scanSeconds float64
}

func NewIndex(root string) *Index {
	return &Index{root: root, byID: map[string]*Album{}}
}

func (ix *Index) Root() string {
	return ix.root
}

func (ix *Index) Scan() error {
	started := time.Now()
	albums, errs := scanRoot(ix.root)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	sort.Slice(albums, func(i, j int) bool { return compareAlbums(albums[i], albums[j]) })

	byID := make(map[string]*Album, len(albums))
	for i := range albums {
		byID[albums[i].ID] = &albums[i]
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.albums = albums
	ix.byID = byID
	ix.scannedAt = time.Now()
	ix.scanSeconds = time.Since(started).Seconds()
	return nil
}

func compareAlbums(a, b Album) bool {
	if strings.ToLower(a.Meta.AlbumArtist) != strings.ToLower(b.Meta.AlbumArtist) {
		return strings.ToLower(a.Meta.AlbumArtist) < strings.ToLower(b.Meta.AlbumArtist)
	}
	if a.Meta.Year != b.Meta.Year {
		return a.Meta.Year < b.Meta.Year
	}
	return strings.ToLower(a.Meta.Album) < strings.ToLower(b.Meta.Album)
}

// upsertAlbum replaces one album in place or inserts it in sort order.
// The caller holds the write lock.
func (ix *Index) upsertAlbum(album Album) {
	for i := range ix.albums {
		if ix.albums[i].ID == album.ID {
			ix.albums[i] = album
			ix.byID[album.ID] = &ix.albums[i]
			return
		}
	}
	ix.albums = append(ix.albums, album)
	sort.Slice(ix.albums, func(i, j int) bool { return compareAlbums(ix.albums[i], ix.albums[j]) })
	ix.byID = make(map[string]*Album, len(ix.albums))
	for i := range ix.albums {
		ix.byID[ix.albums[i].ID] = &ix.albums[i]
	}
}

func (ix *Index) Albums(query Query) []Album {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	matched := []Album{}
	for _, album := range ix.albums {
		if albumMatches(album, query) {
			matched = append(matched, album)
		}
	}
	return matched
}

func (ix *Index) Album(id string) (*Album, error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	album, ok := ix.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return album, nil
}

func (ix *Index) Status() Status {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	pending, files := 0, 0
	for i := range ix.albums {
		album := &ix.albums[i]
		if album.Pending {
			pending++
		}
		files += len(album.Files)
	}
	return Status{
		Version:      Version,
		Library:      ix.root,
		Albums:       len(ix.albums),
		Pending:      pending,
		Files:        files,
		ScannedAt:    ix.scannedAt,
		ScanDuration: ix.scanSeconds,
	}
}

var ErrNotFound = errors.New("album not found")

func albumMatches(album Album, query Query) bool {
	if query.Year != 0 && album.Meta.Year != query.Year {
		return false
	}
	if query.Artist != "" && !strings.Contains(strings.ToLower(album.Meta.AlbumArtist), strings.ToLower(query.Artist)) {
		return false
	}
	if query.Q != "" && !matchesQ(album, strings.ToLower(query.Q)) {
		return false
	}
	return true
}

func matchesQ(album Album, q string) bool {
	if strings.Contains(strings.ToLower(album.Meta.Album), q) {
		return true
	}
	if strings.Contains(strings.ToLower(album.Meta.AlbumArtist), q) {
		return true
	}
	for _, track := range album.Meta.Tracks {
		if strings.Contains(strings.ToLower(track.Title), q) {
			return true
		}
	}
	return false
}
