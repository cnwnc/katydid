package library

import (
	"fmt"
	"path/filepath"
	"sort"

	taglib "go.senan.xyz/taglib"

	"doppel.moe/katydid/internal/sidecar"
)

type Finding struct {
	Album  string `json:"album"`
	Kind   string `json:"kind"`
	File   string `json:"file,omitempty"`
	Detail string `json:"detail,omitempty"`
}

const (
	KindPendingImport = "pending_import"
	KindMissingFile   = "missing_file"
	KindUnlistedFile  = "unlisted_file"
	KindUntagged      = "untagged"
	KindTagDrift      = "tag_drift"
	KindNoReleaseID   = "no_release_id"
)

func (ix *Index) Check() []Finding {
	// Snapshot the album list, then do the tag reads without holding the
	// lock: a full check is slow IO and must not stall scans or imports.
	ix.mu.RLock()
	albums := make([]Album, len(ix.albums))
	copy(albums, ix.albums)
	ix.mu.RUnlock()

	findings := []Finding{}
	for i := range albums {
		album := &albums[i]
		if album.Pending {
			findings = append(findings, Finding{Album: album.ID, Kind: KindPendingImport})
			continue
		}
		findings = append(findings, albumFileFindings(album)...)
		findings = append(findings, ix.tagFindings(album)...)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Album != findings[j].Album {
			return findings[i].Album < findings[j].Album
		}
		return findings[i].Kind < findings[j].Kind
	})
	return findings
}

func (ix *Index) tagFindings(album *Album) []Finding {
	sc, err := sidecar.Load(filepath.Join(ix.root, album.ID))
	if err != nil {
		return []Finding{{Album: album.ID, Kind: KindTagDrift, Detail: "sidecar unreadable: " + err.Error()}}
	}
	if sc.TagState == nil {
		return []Finding{{Album: album.ID, Kind: KindUntagged}}
	}

	findings := []Finding{}

	// Musicbrainz is the source of truth; without a release id the album
	// was never verified against it, so it is potentially sketchy.
	if sc.MusicBrainz.ReleaseID == "" {
		findings = append(findings, Finding{Album: album.ID, Kind: KindNoReleaseID, Detail: "imported without a musicbrainz release id"})
	}

	for _, track := range sc.Tracks {
		if track.Tags == nil {
			continue
		}
		raw, err := taglib.ReadTags(filepath.Join(ix.root, album.ID, track.File))
		if err != nil {
			findings = append(findings, Finding{Album: album.ID, Kind: KindTagDrift, File: track.File, Detail: "unreadable: " + err.Error()})
			continue
		}
		for key, want := range track.Tags {
			have := raw[key]
			if !equalValues(have, want) {
				findings = append(findings, Finding{
					Album:  album.ID,
					Kind:   KindTagDrift,
					File:   track.File,
					Detail: fmt.Sprintf("%s: file has %q, sidecar wants %q", key, have, want),
				})
				break
			}
		}
	}
	return findings
}

func equalValues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func albumFileFindings(album *Album) []Finding {
	findings := []Finding{}

	onDisk := map[string]bool{}
	for _, file := range album.Files {
		onDisk[file] = true
	}
	listed := map[string]bool{}
	for _, track := range album.Meta.Tracks {
		listed[track.File] = true
		if !onDisk[track.File] {
			findings = append(findings, Finding{Album: album.ID, Kind: KindMissingFile, File: track.File})
		}
	}
	for _, file := range album.Files {
		if !listed[file] {
			findings = append(findings, Finding{Album: album.ID, Kind: KindUnlistedFile, File: file})
		}
	}
	return findings
}
