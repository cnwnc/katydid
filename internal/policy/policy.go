// Package policy turns metadata into written tags and path names.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"doppel.moe/katydid/internal/safe"
)

const FileName = "musiclib.yaml"

type Policy struct {
	Name      string   `yaml:"name"`
	Keep      []string `yaml:"keep"`
	FeatSplit bool     `yaml:"feat_split"`
	Dir       string   `yaml:"dir"`
	File      string   `yaml:"file"`
	Cover     string   `yaml:"cover"`
}

type Config struct {
	Default  string            `yaml:"default"`
	Policies map[string]Policy `yaml:"policies"`
}

func Default() Config {
	return Config{
		Default: "default",
		Policies: map[string]Policy{
			"default": {
				Name: "default",
				Keep: []string{
					"TITLE", "ARTIST", "ALBUMARTIST", "ALBUM",
					"TRACKNUMBER", "DISCNUMBER", "DATE", "ORIGINALDATE",
					"GENRE", "LABEL", "CATALOGNUMBER",
					"MUSICBRAINZ_ALBUMID", "MUSICBRAINZ_RELEASEGROUPID", "MUSICBRAINZ_TRACKID",
				},
				FeatSplit: true,
				Dir:       "{albumartist}/{year} - {album}",
				File:      "{track:02d} - {title}{ext}",
				Cover:     "cover.jpg",
			},
		},
	}
}

// Load reads musiclib.yaml. A missing file yields the defaults.
func Load(root string) (Config, error) {
	data, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return Config{}, fmt.Errorf("read %s: %w", filepath.Join(root, FileName), err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", filepath.Join(root, FileName), err)
	}
	if cfg.Default == "" || cfg.Policies == nil {
		return Config{}, fmt.Errorf("parse %s: default and policies are required", filepath.Join(root, FileName))
	}
	if _, ok := cfg.Policies[cfg.Default]; !ok {
		return Config{}, fmt.Errorf("parse %s: default policy %q does not exist", filepath.Join(root, FileName), cfg.Default)
	}
	return cfg, nil
}

// Ensure loads the config and writes the defaults when the file is absent,
// so the library always carries its own visible configuration.
func Ensure(root string) (Config, error) {
	cfg, err := Load(root)
	if err != nil {
		return Config{}, err
	}
	path := filepath.Join(root, FileName)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		data, err := yaml.Marshal(cfg)
		if err != nil {
			return Config{}, fmt.Errorf("marshal defaults: %w", err)
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return Config{}, fmt.Errorf("write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return Config{}, fmt.Errorf("rename %s to %s: %w", tmp, path, err)
		}
	}
	return cfg, nil
}

func (c Config) Policy(name string) (Policy, error) {
	if name == "" {
		name = c.Default
	}
	p, ok := c.Policies[name]
	if !ok {
		return Policy{}, fmt.Errorf("policy %q does not exist", name)
	}
	p.Name = name
	return p, nil
}

var featPattern = regexp.MustCompile(`(?i)^(.+?)\s+(?:feat\.?|ft\.?|featuring)\s+(.+)$`)
var featSeparator = regexp.MustCompile(`(?i)\s*(?:,\s*|\s&\s|\sand\s)`)

// Apply selects the keep-list keys from a raw tag map, splits featuring
// artists out of artist fields, and returns the managed payload. Any key
// outside the keep list is dropped, which is the scrub.
func Apply(p Policy, in map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, key := range p.Keep {
		values, ok := in[key]
		if !ok {
			continue
		}
		values = trimNonEmpty(values)
		if len(values) == 0 {
			continue
		}
		if p.FeatSplit && (key == "ARTIST" || key == "ALBUMARTIST") {
			values = splitFeats(values)
		}
		out[key] = values
	}
	return out
}

func splitFeats(values []string) []string {
	out := []string{}
	for _, value := range values {
		m := featPattern.FindStringSubmatch(value)
		if m == nil {
			out = append(out, value)
			continue
		}
		out = append(out, strings.TrimSpace(m[1]))
		for _, rest := range featSeparator.Split(m[2], -1) {
			if rest = strings.TrimSpace(rest); rest != "" {
				out = append(out, rest)
			}
		}
	}
	return dedupe(out)
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func trimNonEmpty(values []string) []string {
	out := []string{}
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

var fieldPattern = regexp.MustCompile(`\{([a-z]+)(?::(\d+)d)?\}`)

// PlanDir renders the album directory path (one or two components) from the
// dir template. Every dynamic component passes through safe.Name.
func PlanDir(p Policy, albumArtist, album string, year int) (string, error) {
	return render(p.Dir, map[string]string{
		"albumartist": safe.Name(albumArtist, "Unknown Artist"),
		"album":       safe.Name(album, "Unknown Album"),
		"year":        strconv.Itoa(year),
	})
}

// PlanFilename renders one track file name from the file template. When the
// album spans multiple discs, {track} renders as "disc-track". The
// extension is lowercased.
func PlanFilename(p Policy, disc, track int, title, ext string, discTotal int) (string, error) {
	trackDisplay := fmt.Sprintf("%0*d", trackWidth(p.File), track)
	if discTotal > 1 {
		trackDisplay = fmt.Sprintf("%d-%s", disc, trackDisplay)
	}
	name, err := render(p.File, map[string]string{
		"track": trackDisplay,
		"disc":  strconv.Itoa(disc),
		"title": safe.Name(title, "Unknown Title"),
		"ext":   strings.ToLower(ext),
	})
	if err != nil {
		return "", err
	}
	return name, nil
}

func trackWidth(template string) int {
	m := fieldPattern.FindStringSubmatch(template)
	if m == nil || m[1] != "track" || m[2] == "" {
		return 2
	}
	width, err := strconv.Atoi(m[2])
	if err != nil || width <= 0 || width > 6 {
		return 2
	}
	return width
}

func render(template string, fields map[string]string) (string, error) {
	out := fieldPattern.ReplaceAllStringFunc(template, func(match string) string {
		sub := fieldPattern.FindStringSubmatch(match)
		value, ok := fields[sub[1]]
		if !ok {
			return match
		}
		if sub[2] != "" {
			width, err := strconv.Atoi(sub[2])
			if err != nil {
				return match
			}
			if n, err := strconv.Atoi(value); err == nil {
				return fmt.Sprintf("%0*d", width, n)
			}
		}
		return value
	})
	if strings.Contains(out, "{") && strings.Contains(out, "}") {
		return "", fmt.Errorf("template %q has unknown fields", template)
	}
	return out, nil
}

const (
	sepEntry = "\x1d"
	sepKey   = "\x1f"
	sepValue = "\x1e"
)

// CanonicalPayload renders a tag payload deterministically.
func CanonicalPayload(payload map[string][]string) string {
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+sepKey+strings.Join(payload[key], sepValue))
	}
	return strings.Join(parts, sepEntry)
}

// HashTracks hashes the written payload of every track, ordered by file
// name. It is the drift detector recorded in the sidecar.
func HashTracks(files []string, payloads []map[string][]string) string {
	order := make([]int, len(files))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return files[order[a]] < files[order[b]] })
	combined := make([]string, 0, len(files))
	for _, i := range order {
		combined = append(combined, files[i]+sepEntry+CanonicalPayload(payloads[i]))
	}
	sum := sha256.Sum256([]byte(strings.Join(combined, sepEntry)))
	return "sha256:" + hex.EncodeToString(sum[:])
}
