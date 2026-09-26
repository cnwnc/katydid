package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestParseAddSpec(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{Options: []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: "artist", Type: discordgo.ApplicationCommandOptionString, Value: "The Beatles"},
		{Name: "album", Type: discordgo.ApplicationCommandOptionString, Value: "Abbey Road"},
		{Name: "year", Type: discordgo.ApplicationCommandOptionInteger, Value: float64(1969)},
		{Name: "mbid", Type: discordgo.ApplicationCommandOptionString, Value: "grp-9"},
		{Name: ephemeralOption, Type: discordgo.ApplicationCommandOptionBoolean, Value: false},
	}}
	spec := parseAddSpec(data)
	if spec.Artist != "The Beatles" || spec.Album != "Abbey Road" || spec.Year != 1969 || spec.MBID != "grp-9" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Ephemeral {
		t.Fatalf("ephemeral = true, want false")
	}
}

func TestParseAddSpecDefaults(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{Options: []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: "artist", Type: discordgo.ApplicationCommandOptionString, Value: "A"},
		{Name: "album", Type: discordgo.ApplicationCommandOptionString, Value: "B"},
	}}
	spec := parseAddSpec(data)
	if spec.Year != 0 || spec.MBID != "" || !spec.Ephemeral {
		t.Fatalf("spec = %+v, want zero fields and ephemeral", spec)
	}
}

func TestParseWantSpec(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{Options: []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: "id", Type: discordgo.ApplicationCommandOptionString, Value: "want-9"},
	}}
	spec := parseWantSpec(data)
	if spec.ID != "want-9" || !spec.Ephemeral {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestOptionBoolWrongType(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{Options: []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: ephemeralOption, Type: discordgo.ApplicationCommandOptionString, Value: "not a bool"},
	}}
	if !optionBool(data, ephemeralOption, true) {
		t.Fatalf("expected fallback true on wrong value type")
	}
}
