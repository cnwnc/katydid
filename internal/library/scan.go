package library

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"doppel.moe/katydid/internal/sidecar"
	"doppel.moe/katydid/internal/tags"
)

var audioExtensions = map[string]bool{
	".flac": true,
	".mp3":  true,
	".ogg":  true,
	".oga":  true,
	".m4a":  true,
	".opus": true,
	".wav":  true,
	".wma":  true,
}

const scanWorkers = 8

func IsAudio(name string) bool {
	return audioExtensions[strings.ToLower(filepath.Ext(name))]
}

func scanRoot(root string) ([]Album, []error) {
	dirs, err := albumDirs(root)
	if err != nil {
		return nil, []error{err}
	}

	albums := make([]Album, len(dirs))
	errs := []error{}
	var errsMu sync.Mutex

	sem := make(chan struct{}, scanWorkers)
	var wg sync.WaitGroup
	for i, dir := range dirs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, dir albumDir) {
			defer wg.Done()
			defer func() { <-sem }()
			album, albumErrs := buildAlbum(root, dir)
			albums[i] = album
			if len(albumErrs) > 0 {
				errsMu.Lock()
				errs = append(errs, albumErrs...)
				errsMu.Unlock()
			}
		}(i, dir)
	}
	wg.Wait()
	return albums, errs
}

type albumDir struct {
	rel   string
	abs   string
	files []string
}

func albumDirs(root string) ([]albumDir, error) {
	dirs := map[string]*albumDir{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !audioExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			return nil
		}
		abs := filepath.Dir(path)
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return fmt.Errorf("relativize %s: %w", abs, err)
		}
		if dirs[rel] == nil {
			dirs[rel] = &albumDir{rel: rel, abs: abs}
		}
		dirs[rel].files = append(dirs[rel].files, filepath.Base(path))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}

	out := make([]albumDir, 0, len(dirs))
	for _, dir := range dirs {
		sort.Strings(dir.files)
		out = append(out, *dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

func buildAlbum(root string, dir albumDir) (Album, []error) {
	album := Album{ID: dir.rel, Files: dir.files}

	sc, err := sidecar.Load(dir.abs)
	switch {
	case err == nil:
		album.Meta = sidecarMeta(sc)
		return album, nil
	case errors.Is(err, sidecar.ErrNoSidecar):
		album.Pending = true
		album.Meta = tagsMeta(dir.abs, dir.files)
		return album, nil
	default:
		return album, []error{err}
	}
}

func sidecarMeta(sc *sidecar.Album) Meta {
	tracks := make([]TrackMeta, 0, len(sc.Tracks))
	for _, track := range sc.Tracks {
		tracks = append(tracks, TrackMeta{
			File:          track.File,
			Title:         track.Title,
			Track:         track.Track,
			Disc:          track.Disc,
			LengthSeconds: track.LengthSeconds,
		})
	}
	return Meta{AlbumArtist: sc.AlbumArtist, Album: sc.Album, Year: sc.Year, ReleaseID: sc.MusicBrainz.ReleaseID, By: sc.Provenance.By, Request: sc.Provenance.Request, Tracks: tracks}
}

type fileMeta struct {
	TrackMeta
	album       string
	albumArtist string
	year        int
}

func tagsMeta(abs string, files []string) Meta {
	results := make([]fileMeta, len(files))

	sem := make(chan struct{}, scanWorkers)
	var wg sync.WaitGroup
	for i, file := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, file string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = trackMetaFromFile(abs, file)
		}(i, file)
	}
	wg.Wait()

	tracks := make([]TrackMeta, 0, len(results))
	meta := Meta{}
	for _, r := range results {
		tracks = append(tracks, r.TrackMeta)
		if meta.Album == "" {
			meta.Album = r.album
		}
		if meta.AlbumArtist == "" {
			meta.AlbumArtist = r.albumArtist
		}
		if meta.Year == 0 {
			meta.Year = r.year
		}
	}
	sort.Slice(tracks, func(i, j int) bool {
		a, b := tracks[i], tracks[j]
		if a.Disc != b.Disc {
			return a.Disc < b.Disc
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		return a.File < b.File
	})
	meta.Tracks = tracks
	return meta
}

func trackMetaFromFile(absDir, file string) fileMeta {
	meta := fileMeta{}
	meta.File = file
	meta.Title = strings.TrimSuffix(file, filepath.Ext(file))

	fileTags, err := tags.Read(filepath.Join(absDir, file))
	if err != nil {
		return meta
	}
	meta.Title = fileTags.Title
	meta.Track = fileTags.TrackNumber
	meta.Disc = fileTags.DiscNumber
	meta.LengthSeconds = fileTags.LengthSeconds
	meta.album = fileTags.Album
	if len(fileTags.AlbumArtists) > 0 {
		meta.albumArtist = fileTags.AlbumArtists[0]
	} else if len(fileTags.Artists) > 0 {
		meta.albumArtist = fileTags.Artists[0]
	}
	meta.year = fileTags.Year
	return meta
}

// RescanDir refreshes a single album directory, for use after targeted
// writes like publish or retag; a full Scan is only needed when the
// directory set itself changes.
func (ix *Index) RescanDir(rel string) error {
	abs := filepath.Join(ix.root, rel)
	files := []string{}
	err := filepath.WalkDir(abs, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != abs && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !IsAudio(entry.Name()) || filepath.Dir(path) != abs {
			return nil
		}
		files = append(files, entry.Name())
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", abs, err)
	}
	sort.Strings(files)

	album, errs := buildAlbum(ix.root, albumDir{rel: rel, abs: abs, files: files})
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.upsertAlbum(album)
	return nil
}
