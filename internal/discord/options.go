package discord

import (
	"github.com/bwmarrin/discordgo"

	"doppel.moe/katydid/internal/lastfm"
)

type addSpec struct {
	Artist    string
	Album     string
	Year      int
	MBID      string
	Ephemeral bool
	// Candidates holds the resolve results behind this prompt so a pick
	// can seed the want with musicbrainz names before fetchd resolves.
	Candidates []Candidate
	// LastFM carries the last.fm album behind an unvetted import prompt.
	LastFM *lastfm.Album
}

func parseAddSpec(data discordgo.ApplicationCommandInteractionData) addSpec {
	return addSpec{
		Artist:    optionString(data, "artist"),
		Album:     optionString(data, "album"),
		Year:      optionInt(data, "year"),
		MBID:      optionString(data, "mbid"),
		Ephemeral: optionBool(data, ephemeralOption, true),
	}
}

type wantSpec struct {
	ID        string
	Ephemeral bool
}

func parseWantSpec(data discordgo.ApplicationCommandInteractionData) wantSpec {
	return wantSpec{
		ID:        optionString(data, "id"),
		Ephemeral: optionBool(data, ephemeralOption, true),
	}
}

// The option readers below never panic, unlike the discordgo accessors.

func optionString(data discordgo.ApplicationCommandInteractionData, name string) string {
	opt := data.GetOption(name)
	if opt == nil {
		return ""
	}
	s, _ := opt.Value.(string)
	return s
}

func optionInt(data discordgo.ApplicationCommandInteractionData, name string) int {
	opt := data.GetOption(name)
	if opt == nil {
		return 0
	}
	f, _ := opt.Value.(float64)
	return int(f)
}

func optionBool(data discordgo.ApplicationCommandInteractionData, name string, fallback bool) bool {
	opt := data.GetOption(name)
	if opt == nil {
		return fallback
	}
	b, ok := opt.Value.(bool)
	if !ok {
		return fallback
	}
	return b
}
