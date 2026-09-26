package discord

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	pollInterval = 5 * time.Second
	pollBudget   = 14 * time.Minute
	// tracked maps are capped with a blunt reset; entries are tiny and
	// a lost guard after a reset only risks one duplicate click.
	mapCap = 4096
)

type Config struct {
	AppID   string
	GuildID string
}

type Bot struct {
	katyd   *Katyd
	fetchd  *Fetchd
	appID   string
	guildID string
	cmds    []*discordgo.ApplicationCommand

	mu        sync.Mutex
	pending   map[string]addSpec            // followup message id -> original command spec
	polls     map[string]context.CancelFunc // interaction token -> poll cancel
	processed map[string]bool               // component message ids already consumed
	root      context.Context
	cancel    context.CancelFunc
}

func NewBot(katyd *Katyd, fetchd *Fetchd, cfg Config) *Bot {
	ctx, cancel := context.WithCancel(context.Background())
	return &Bot{
		katyd:     katyd,
		fetchd:    fetchd,
		appID:     cfg.AppID,
		guildID:   cfg.GuildID,
		cmds:      commands(),
		pending:   map[string]addSpec{},
		polls:     map[string]context.CancelFunc{},
		processed: map[string]bool{},
		root:      ctx,
		cancel:    cancel,
	}
}

// Close cancels every running poll goroutine.
func (b *Bot) Close() { b.cancel() }

// OnReady registers the slash commands; guild id empty means global.
func (b *Bot) OnReady(s *discordgo.Session, _ *discordgo.Ready) {
	cmds, err := s.ApplicationCommandBulkOverwrite(b.appID, b.guildID, b.cmds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "katy-discordd: register commands: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "katy-discordd: registered %d commands (guild %q)\n", len(cmds), b.guildID)
}

// OnInteraction dispatches slash commands and component clicks.
func (b *Bot) OnInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "katy-discordd: interaction panic: %v\n", r)
		}
	}()
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		b.onCommand(s, i)
	case discordgo.InteractionMessageComponent:
		b.onComponent(s, i)
	}
}

func (b *Bot) onCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	switch data.Name {
	case "addalbum":
		b.onAddAlbum(s, i, parseAddSpec(data))
	case "wants":
		b.onWants(s, i, optionBool(data, ephemeralOption, true))
	case "want":
		b.onWant(s, i, parseWantSpec(data))
	default:
		fmt.Fprintf(os.Stderr, "katy-discordd: unknown command %q\n", data.Name)
	}
}

func (b *Bot) onAddAlbum(s *discordgo.Session, i *discordgo.InteractionCreate, spec addSpec) {
	if spec.Artist == "" || spec.Album == "" {
		b.deferRespond(s, i, spec.Ephemeral)
		b.editOriginal(s, i.Interaction, "artist and album are required")
		return
	}
	if err := b.deferRespond(s, i, spec.Ephemeral); err != nil {
		fmt.Fprintf(os.Stderr, "katy-discordd: defer addalbum: %v\n", err)
		return
	}
	candidates, auto, err := b.katyd.Resolve(Query{
		Artist: spec.Artist,
		Album:  spec.Album,
		Year:   spec.Year,
		MBID:   spec.MBID,
		Limit:  defaultMenuSize,
	})
	if err != nil {
		b.editOriginal(s, i.Interaction, err.Error())
		return
	}
	switch {
	case len(candidates) == 0:
		b.editOriginal(s, i.Interaction, fmt.Sprintf("No matches for %s - %s.", spec.Artist, spec.Album))
	case len(candidates) == 1:
		b.followSingle(s, i, spec, candidates[0], auto)
	default:
		b.followMenu(s, i, spec, candidates, defaultMenuSize)
	}
}

// followSingle offers add/cancel buttons for the one candidate.
func (b *Bot) followSingle(s *discordgo.Session, i *discordgo.InteractionCreate, spec addSpec, c Candidate, auto bool) {
	msg, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:         matchText(c, auto),
		Flags:           ephemeralFlags(spec.Ephemeral),
		AllowedMentions: noMentions(),
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.Button{Label: addButtonLabel, Style: discordgo.PrimaryButton, CustomID: customAdd + c.ReleaseID},
				discordgo.Button{Label: cancelButtonLabel, Style: discordgo.SecondaryButton, CustomID: customCancel},
			}},
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "katy-discordd: addalbum followup: %v\n", err)
		return
	}
	b.trackPending(msg.ID, spec)
}

// followMenu offers a release group select menu plus a show-more button.
func (b *Bot) followMenu(s *discordgo.Session, i *discordgo.InteractionCreate, spec addSpec, candidates []Candidate, limit int) {
	msg, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:         choicesText(spec, len(candidates)),
		Flags:           ephemeralFlags(spec.Ephemeral),
		AllowedMentions: noMentions(),
		Components:      menuComponents(candidates, limit),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "katy-discordd: addalbum followup: %v\n", err)
		return
	}
	b.trackPending(msg.ID, spec)
}

func menuComponents(candidates []Candidate, limit int) []discordgo.MessageComponent {
	menu := discordgo.SelectMenu{CustomID: customPick, Placeholder: "Pick a release group", Options: menuOptions(candidates, limit)}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{menu}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: expandButtonLabel, Style: discordgo.SecondaryButton, CustomID: customExpand},
		}},
	}
}

func (b *Bot) onWants(s *discordgo.Session, i *discordgo.InteractionCreate, ephemeral bool) {
	if err := b.deferRespond(s, i, ephemeral); err != nil {
		fmt.Fprintf(os.Stderr, "katy-discordd: defer wants: %v\n", err)
		return
	}
	wants, err := b.fetchd.Wants()
	if err != nil {
		b.editOriginal(s, i.Interaction, err.Error())
		return
	}
	b.editOriginal(s, i.Interaction, listText(wants))
}

func (b *Bot) onWant(s *discordgo.Session, i *discordgo.InteractionCreate, spec wantSpec) {
	if err := b.deferRespond(s, i, spec.Ephemeral); err != nil {
		fmt.Fprintf(os.Stderr, "katy-discordd: defer want: %v\n", err)
		return
	}
	if spec.ID == "" {
		b.editOriginal(s, i.Interaction, "want id is required")
		return
	}
	w, err := b.fetchd.Want(spec.ID)
	if err != nil {
		b.editOriginal(s, i.Interaction, err.Error())
		return
	}
	b.editOriginal(s, i.Interaction, wantText(w))
}

func (b *Bot) onComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	act := parseAction(data.CustomID, data.Values)
	switch act.kind {
	case actionCancel:
		b.consume(i.Message.ID)
		b.updateMessage(s, i, "Cancelled.", nil)
	case actionExpand:
		b.onExpand(s, i)
	case actionAdd, actionPick:
		if b.consume(i.Message.ID) {
			b.updateMessage(s, i, "Already handled.", nil)
			return
		}
		b.onPick(s, i, act.value)
	default:
		fmt.Fprintf(os.Stderr, "katy-discordd: unknown component %q\n", data.CustomID)
	}
}

// onExpand re-resolves with the bigger menu on the same message.
func (b *Bot) onExpand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	spec, ok := b.pendingFor(i.Message.ID)
	if !ok {
		spec = parseChoicesSpec(messageText(i.Message))
	}
	if spec.Artist == "" || spec.Album == "" {
		b.updateMessage(s, i, "This prompt expired — run /addalbum again.", nil)
		return
	}
	candidates, _, err := b.katyd.Resolve(Query{
		Artist: spec.Artist,
		Album:  spec.Album,
		Year:   spec.Year,
		MBID:   spec.MBID,
		Limit:  maxMenuSize,
	})
	if err != nil {
		b.updateMessage(s, i, err.Error(), nil)
		return
	}
	b.updateMessage(s, i, choicesText(spec, len(candidates)), menuComponents(candidates, maxMenuSize))
}

// onPick queues the picked release group and starts the poll loop.
func (b *Bot) onPick(s *discordgo.Session, i *discordgo.InteractionCreate, releaseID string) {
	if releaseID == "" {
		b.updateMessage(s, i, "Empty selection.", nil)
		return
	}
	spec, ok := b.pendingFor(i.Message.ID)
	if !ok {
		spec = fallbackSpec(i)
	}
	if spec.Artist == "" || spec.Album == "" {
		b.updateMessage(s, i, "This prompt expired — run /addalbum again.", nil)
		return
	}
	var label string
	if i.Message != nil {
		label = parseMatchLabel(messageText(i.Message))
	}
	b.updateMessage(s, i, queuedLine(spec, label), nil)

	add := AddWant{Artist: spec.Artist, Album: spec.Album, Year: spec.Year, Group: releaseID}
	if spec.MBID != "" {
		add.MBID, add.Group = releaseID, ""
	}
	want, err := b.fetchd.Add(add)
	if err != nil {
		b.updateWebhook(s, i.Interaction.Token, componentMessageID(i), err.Error())
		return
	}
	b.updateWebhook(s, i.Interaction.Token, componentMessageID(i), statusLine(want))
	b.startPoll(s, i.Interaction.Token, want)
}

// poll drives the deferred response (@original) with live want status.
func (b *Bot) poll(s *discordgo.Session, ctx context.Context, token, wantID string) {
	defer b.untrack(token)
	deadline := time.NewTimer(pollBudget)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			b.updateWebhook(s, token, "@original", timeoutLine(wantID))
			return
		case <-ticker.C:
		}
		w, err := b.fetchd.Want(wantID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "katy-discordd: poll want %s: %v\n", wantID, err)
			continue
		}
		if line := statusLine(w); line != last {
			if err := b.updateWebhook(s, token, "@original", line); err != nil {
				fmt.Fprintf(os.Stderr, "katy-discordd: poll edit %s: %v\n", wantID, err)
				continue
			}
			last = line
		}
		if terminalState(w.State) {
			return
		}
	}
}

func (b *Bot) startPoll(s *discordgo.Session, token string, want Want) {
	b.mu.Lock()
	if _, dup := b.polls[token]; dup {
		b.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(b.root)
	b.polls[token] = cancel
	b.mu.Unlock()
	go b.poll(s, ctx, token, want.ID)
}

func (b *Bot) untrack(token string) {
	b.mu.Lock()
	delete(b.polls, token)
	b.mu.Unlock()
}

func (b *Bot) trackPending(messageID string, spec addSpec) {
	b.mu.Lock()
	defer capMap(b.pending, mapCap)
	b.pending[messageID] = spec
	b.mu.Unlock()
}

func (b *Bot) pendingFor(messageID string) (addSpec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	spec, ok := b.pending[messageID]
	return spec, ok
}

// consume marks a component message handled and reports a double click.
func (b *Bot) consume(messageID string) bool {
	b.mu.Lock()
	defer capMap(b.processed, mapCap)
	defer b.mu.Unlock()
	if b.processed[messageID] {
		return true
	}
	b.processed[messageID] = true
	delete(b.pending, messageID)
	return false
}

func capMap[V any](m map[string]V, cap int) {
	if len(m) >= cap {
		for k := range m {
			delete(m, k)
		}
	}
}

func (b *Bot) deferRespond(s *discordgo.Session, i *discordgo.InteractionCreate, ephemeral bool) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: ephemeralFlags(ephemeral)},
	})
}

// updateMessage answers a component interaction by editing its host message.
func (b *Bot) updateMessage(s *discordgo.Session, i *discordgo.InteractionCreate, text string, components []discordgo.MessageComponent) {
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:         text,
			Components:      components,
			AllowedMentions: noMentions(),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "katy-discordd: update component message: %v\n", err)
	}
}

// updateWebhook patches a webhook message (@original or a followup) by token.
func (b *Bot) updateWebhook(s *discordgo.Session, token, messageID, text string) error {
	_, err := s.WebhookMessageEdit(b.appID, token, messageID, &discordgo.WebhookEdit{
		Content:         &text,
		AllowedMentions: noMentions(),
	})
	return err
}

func (b *Bot) editOriginal(s *discordgo.Session, i *discordgo.Interaction, text string) {
	if _, err := s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
		Content:         &text,
		AllowedMentions: noMentions(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "katy-discordd: edit response: %v\n", err)
	}
}

// fallbackSpec recovers the command spec from the message when local state is
// gone, e.g. after a restart. It defaults to the group pin because the mbid
// flag is not recoverable from message text.
func fallbackSpec(i *discordgo.InteractionCreate) addSpec {
	if i.Message == nil {
		return addSpec{}
	}
	if spec := parseChoicesSpec(messageText(i.Message)); spec.Artist != "" {
		return spec
	}
	if label := parseMatchLabel(messageText(i.Message)); label != "" {
		if artist, album, ok := splitArtistAlbum(label); ok {
			return addSpec{Artist: artist, Album: album}
		}
	}
	return addSpec{}
}

// componentMessageID is the followup message hosting the clicked component.
func componentMessageID(i *discordgo.InteractionCreate) string {
	if i.Message != nil {
		return i.Message.ID
	}
	return "@original"
}

func messageText(m *discordgo.Message) string {
	if m == nil {
		return ""
	}
	return m.Content
}

func ephemeralFlags(ephemeral bool) discordgo.MessageFlags {
	if ephemeral {
		return discordgo.MessageFlagsEphemeral
	}
	return 0
}

// noMentions keeps user-supplied text from pinging anyone.
func noMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}
