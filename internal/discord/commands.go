package discord

import "github.com/bwmarrin/discordgo"

const ephemeralOption = "ephemeral"

// installTypes and contexts make the commands work for user installs
// and dms, not just servers; unset, discord scopes them to the guild
// install only and the user-install command list is the fragile one.
var (
	installTypes = &[]discordgo.ApplicationIntegrationType{
		discordgo.ApplicationIntegrationGuildInstall,
		discordgo.ApplicationIntegrationUserInstall,
	}
	contexts = &[]discordgo.InteractionContextType{
		discordgo.InteractionContextGuild,
		discordgo.InteractionContextBotDM,
		discordgo.InteractionContextPrivateChannel,
	}
)

func commands() []*discordgo.ApplicationCommand {
	ephemeral := &discordgo.ApplicationCommandOption{
		Type:        discordgo.ApplicationCommandOptionBoolean,
		Name:        ephemeralOption,
		Description: "reply visible only to you (default true)",
	}
	return []*discordgo.ApplicationCommand{
		{
			Name:             "addalbum",
			Description:      "queue a fetch for an album by musicbrainz match",
			IntegrationTypes: installTypes,
			Contexts:         contexts,
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "artist", Description: "artist name", Required: true},
				{Type: discordgo.ApplicationCommandOptionString, Name: "album", Description: "album title", Required: true},
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "year", Description: "release year"},
				{Type: discordgo.ApplicationCommandOptionString, Name: "mbid", Description: "musicbrainz release group id"},
				ephemeral,
			},
		},
		{
			Name:             "wants",
			Description:      "list queued fetch wants",
			IntegrationTypes: installTypes,
			Contexts:         contexts,
			Options:          []*discordgo.ApplicationCommandOption{ephemeral},
		},
		{
			Name:             "want",
			Description:      "inspect one fetch want by id",
			IntegrationTypes: installTypes,
			Contexts:         contexts,
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "id", Description: "want id", Required: true},
				ephemeral,
			},
		},
	}
}
