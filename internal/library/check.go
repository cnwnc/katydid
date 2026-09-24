package library

import "sort"

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
)

func (ix *Index) Check() []Finding {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	findings := []Finding{}
	for i := range ix.albums {
		album := &ix.albums[i]
		if album.Pending {
			findings = append(findings, Finding{Album: album.ID, Kind: KindPendingImport})
			continue
		}
		findings = append(findings, albumFileFindings(album)...)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Album != findings[j].Album {
			return findings[i].Album < findings[j].Album
		}
		return findings[i].Kind < findings[j].Kind
	})
	return findings
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
