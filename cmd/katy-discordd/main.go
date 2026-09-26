package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"

	"doppel.moe/katydid/internal/discord"
	"doppel.moe/katydid/internal/navidrome"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "katy-discordd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	defaultToken := os.Getenv("KATYDISCORD_TOKEN")
	defaultAppID := os.Getenv("KATYDISCORD_APP_ID")
	defaultGuild := os.Getenv("KATYDISCORD_GUILD")
	defaultFetchd := os.Getenv("KATYFETCHD_SOCKET")
	if defaultFetchd == "" {
		defaultFetchd = "/run/katy-fetchd/fetchd.sock"
	}
	defaultKatyd := os.Getenv("KATYDID_SOCKET")
	if defaultKatyd == "" {
		defaultKatyd = os.Getenv("KATYDISCORD_KATYD_SOCKET")
	}
	if defaultKatyd == "" {
		defaultKatyd = "/run/katyd/katyd.sock"
	}
	defaultNavUser := os.Getenv("NAVIDROME_USERNAME")
	defaultNavPass := os.Getenv("NAVIDROME_PASSWORD")
	defaultNavBase := os.Getenv("NAVIDROME_BASE_URL")
	defaultNavPort := os.Getenv("NAVIDROME_PORT")

	token := flag.String("token", defaultToken, "discord bot token")
	appID := flag.String("app-id", defaultAppID, "discord application id")
	guild := flag.String("guild", defaultGuild, "discord guild id (empty registers globally)")
	fetchdSocket := flag.String("fetchd-socket", defaultFetchd, "katy-fetchd unix socket path")
	katydSocket := flag.String("katyd-socket", defaultKatyd, "katyd unix socket path")
	flag.Parse()

	if *token == "" {
		return errors.New("no discord token: set -token or KATYDISCORD_TOKEN")
	}
	if *appID == "" {
		return errors.New("no application id: set -app-id or KATYDISCORD_APP_ID")
	}

	session, err := discordgo.New("Bot " + *token)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	session.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsGuilds)

	var navd discord.Navidrome
	navdStatus := "disabled"
	if defaultNavUser != "" && defaultNavPass != "" {
		base, err := navidrome.BaseURL(defaultNavBase, defaultNavPort)
		if err != nil {
			return err
		}
		navd = navidrome.New(base, defaultNavUser, defaultNavPass)
		navdStatus = base
	}

	bot := discord.NewBot(discord.NewKatyd(*katydSocket), discord.NewFetchd(*fetchdSocket), navd,
		discord.Config{AppID: *appID, GuildID: *guild})
	session.AddHandler(bot.OnReady)
	session.AddHandler(bot.OnInteraction)

	if err := session.Open(); err != nil {
		return fmt.Errorf("open gateway: %w", err)
	}

	fmt.Fprintf(os.Stderr, "katy-discordd: app %s, guild %q, katyd %s, fetchd %s, navidrome %s\n",
		*appID, *guild, *katydSocket, *fetchdSocket, navdStatus)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	bot.Close()
	if err := session.Close(); err != nil {
		return fmt.Errorf("close gateway: %w", err)
	}
	return nil
}
