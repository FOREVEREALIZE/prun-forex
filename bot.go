package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const discordAPI = "https://discord.com/api/v10"

// Discord JSON error code for "Cannot send messages to this user".
const errCannotDM = 50007

type Bot struct {
	appID  string
	token  string
	pubKey ed25519.PublicKey
	store  *Store
	fnar   *FNAR
	api    string // Discord API base URL
	http   *http.Client
	async  sync.WaitGroup // deferred interaction work, so tests can wait for it

	mu         sync.Mutex
	dmChannels map[string]string // discord user id -> DM channel id
}

func NewBot(appID, token, publicKeyHex string, store *Store, fnar *FNAR) (*Bot, error) {
	b := &Bot{appID: appID, token: token, store: store, fnar: fnar, api: discordAPI,
		http: &http.Client{Timeout: 15 * time.Second}, dmChannels: map[string]string{}}
	if publicKeyHex != "" {
		k, err := hex.DecodeString(publicKeyHex)
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("DISCORD_PUBLIC_KEY must be a %d-byte hex key", ed25519.PublicKeySize)
		}
		b.pubKey = k
	}
	return b, nil
}

type discordError struct {
	Status     int
	Code       int     `json:"code"`
	Message    string  `json:"message"`
	RetryAfter float64 `json:"retry_after"`
}

func (e *discordError) Error() string {
	return fmt.Sprintf("discord %d (code %d): %s", e.Status, e.Code, e.Message)
}

func (b *Bot) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.api+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+b.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "DiscordBot (prun-forex, 1.0)")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		de := &discordError{Status: resp.StatusCode}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(de)
		return de
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// RegisterCommands replaces the app's global commands, all usable in a DM
// with the bot, whether the app is installed on a user or a server.
func (b *Bot) RegisterCommands(ctx context.Context) error {
	return b.call(ctx, http.MethodPut, "/applications/"+b.appID+"/commands", b.commands(), nil)
}

type interaction struct {
	Type          int    `json:"type"`
	Token         string `json:"token"`
	ApplicationID string `json:"application_id"`
	GuildID       string `json:"guild_id"`
	Data          struct {
		Name     string          `json:"name"`
		CustomID string          `json:"custom_id"`
		Options  []commandOption `json:"options"`
	} `json:"data"`
	User   *discordUser `json:"user"`
	Member *struct {
		User        *discordUser `json:"user"`
		Permissions string       `json:"permissions"`
	} `json:"member"`
	// Context is 0 in a server, 1 in the bot's own DM, 2 in any other DM.
	Context *int `json:"context"`
}

func (in interaction) inBotDM() bool { return in.Context == nil || *in.Context == 1 }

// publicCommands post where everyone in the channel can see them. The rest
// stay with the person who ran them: their own orders, trades and anything
// with buttons only they should press.
var publicCommands = map[string]bool{"orders": true}

// ephemeral decides whether a reply is visible only to the person who asked.
// In the bot's own DM there's nobody else to hide it from.
func (in interaction) ephemeral() bool {
	if in.inBotDM() {
		return false
	}
	if publicCommands[in.Data.Name] {
		return in.optionBool("private")
	}
	return true
}

const ephemeralFlag = 64

type commandOption struct {
	Name    string          `json:"name"`
	Type    int             `json:"type"`
	Value   json.RawMessage `json:"value"`
	Options []commandOption `json:"options"`
}

// sub returns the subcommand's name and options, for commands that have them.
func (in interaction) sub() (string, []commandOption) {
	for _, o := range in.Data.Options {
		if o.Type == 1 { // subcommand
			return o.Name, o.Options
		}
	}
	return "", in.Data.Options
}

func (in interaction) optionRaw(name string) json.RawMessage {
	_, opts := in.sub()
	for _, o := range opts {
		if o.Name == name {
			return o.Value
		}
	}
	return nil
}

// canManageGuild reports whether the caller may set up boards: Manage Server
// or Administrator. Discord hides the command from everyone else, but the
// check is here too.
func (in interaction) canManageGuild() bool {
	if in.Member == nil {
		return false
	}
	perms, err := strconv.ParseUint(in.Member.Permissions, 10, 64)
	if err != nil {
		return false
	}
	const manageGuild, administrator = 1 << 5, 1 << 3
	return perms&(manageGuild|administrator) != 0
}

func (in interaction) option(name string) string {
	var v string
	json.Unmarshal(in.optionRaw(name), &v)
	return v
}

func (in interaction) optionInt(name string) int64 {
	var v int64
	json.Unmarshal(in.optionRaw(name), &v)
	return v
}

func (in interaction) optionBool(name string) bool {
	var v bool
	json.Unmarshal(in.optionRaw(name), &v)
	return v
}

func (b *Bot) verify(r *http.Request, body []byte) bool {
	sig, err := hex.DecodeString(r.Header.Get("X-Signature-Ed25519"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	msg := append([]byte(r.Header.Get("X-Signature-Timestamp")), body...)
	return ed25519.Verify(b.pubKey, msg, sig)
}

func (b *Bot) HandleInteraction(w http.ResponseWriter, r *http.Request) {
	if b.pubKey == nil {
		http.Error(w, "interactions not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !b.verify(r, body) {
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return
	}
	var in interaction
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	switch in.Type {
	case 1: // PING
		writeJSON(w, map[string]int{"type": 1})
		return
	case 2, 3: // APPLICATION_COMMAND, MESSAGE_COMPONENT
	default:
		http.Error(w, "unsupported interaction", http.StatusBadRequest)
		return
	}

	user := in.User
	if user == nil && in.Member != nil {
		user = in.Member.User
	}
	if user == nil {
		reply(w, in, "Couldn't tell who you are.")
		return
	}
	if in.Type == 3 {
		b.handleButton(w, in, *user)
		return
	}
	if in.Data.Name == "board" {
		b.deferred(w, in, 5, func(ctx context.Context) botMessage { return b.cmdBoard(ctx, in) })
		return
	}
	ctx := r.Context()
	switch in.Data.Name {
	case "link":
		if !in.inBotDM() {
			reply(w, in, "Run `/link` in a DM with me, not here.")
			return
		}
		code := NormalizeCompanyCode(in.option("company_code"))
		if code == "" {
			reply(w, in, "Give your company code (1–4 letters), e.g. `/link company_code:NIKU`.")
			return
		}
		b.deferred(w, in, 5, func(ctx context.Context) botMessage { return b.confirmLink(ctx, *user, code) })
	case "unlink":
		if err := b.store.Unlink(ctx, user.ID); err != nil {
			log.Printf("unlink %s: %v", user.ID, err)
			reply(w, in, "Something went wrong, try again in a moment.")
			return
		}
		reply(w, in, "Unlinked. I won't DM you anymore. Run `/link company_code:ABCD` to trade again.")
	default:
		b.deferred(w, in, 5, func(ctx context.Context) botMessage { return b.command(ctx, in, *user) })
	}
}

type botMessage struct {
	Content         string         `json:"content"`
	Components      []any          `json:"components"` // empty removes buttons
	AllowedMentions map[string]any `json:"allowed_mentions"`
}

// deferred acknowledges an interaction right away (ackType 5 for a new reply,
// 6 to update the message a button is on), does work that may take longer
// than Discord's 3 seconds, then edits the message with the result.
func (b *Bot) deferred(w http.ResponseWriter, in interaction, ackType int, work func(context.Context) botMessage) {
	ack := map[string]any{"type": ackType}
	// Only a new reply can choose; an update keeps the original message's flags.
	if ackType == 5 && in.ephemeral() {
		ack["data"] = map[string]any{"flags": ephemeralFlag}
	}
	writeJSON(w, ack)
	b.async.Add(1)
	go func() {
		defer b.async.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		msg := work(ctx)
		if msg.Components == nil {
			msg.Components = []any{}
		}
		msg.AllowedMentions = map[string]any{"parse": []string{}}
		if err := b.call(ctx, http.MethodPatch, "/webhooks/"+in.ApplicationID+"/"+in.Token+"/messages/@original", msg, nil); err != nil {
			log.Printf("interaction %s: editing reply: %v", in.Data.Name+in.Data.CustomID, err)
		}
	}()
}

// lookupCompany finds a company on FNAR for user, or returns the reply
// explaining why it can't be linked.
func (b *Bot) lookupCompany(ctx context.Context, user discordUser, code string) (Company, string) {
	c, err := b.fnar.Company(ctx, code)
	switch {
	case errors.Is(err, ErrNoCompany):
		return c, fmt.Sprintf("Couldn't find a company with code `%s` on FNAR. Check the code and run `/link` again.", code)
	case err != nil:
		log.Printf("link %s: fnar: %v", user.ID, err)
		return c, "Couldn't reach FNAR to look up your company. Try again in a minute."
	}
	taken, err := b.store.CompanyTaken(ctx, c.Code, user.ID)
	switch {
	case err != nil:
		log.Printf("link %s: %v", user.ID, err)
		return c, "Something went wrong, try again in a moment."
	case taken:
		return c, fmt.Sprintf("`%s` is already linked to another Discord account.", c.Code)
	}
	return c, ""
}

// confirmLink is the first step of /link: show who FNAR says owns the company
// and ask the user to confirm.
func (b *Bot) confirmLink(ctx context.Context, user discordUser, code string) botMessage {
	c, problem := b.lookupCompany(ctx, user, code)
	if problem != "" {
		return botMessage{Content: problem}
	}
	msg := fmt.Sprintf("You are **%s** and own **%s** (`%s`)", c.UserName, c.Name, c.Code)
	if c.CorpCode != "" {
		msg += fmt.Sprintf(", a part of **%s** (`%s`)", c.CorpName, c.CorpCode)
	}
	msg += ". Is this correct?"
	return botMessage{Content: msg, Components: []any{map[string]any{
		"type": 1, // action row
		"components": []any{
			map[string]any{"type": 2, "style": 3, "label": "Yes", "custom_id": "link:yes:" + c.Code},
			map[string]any{"type": 2, "style": 4, "label": "No", "custom_id": "link:no"},
		},
	}}}
}

// handleButton handles the Yes/No buttons from confirmLink.
func (b *Bot) handleButton(w http.ResponseWriter, in interaction, user discordUser) {
	update := func(content string) {
		writeJSON(w, map[string]any{"type": 7, "data": map[string]any{"content": content, "components": []any{}}})
	}
	id := in.Data.CustomID
	switch {
	case id == "link:no":
		update("Okay, nothing was linked. Run `/link` again with the right company code.")
	case strings.HasPrefix(id, "link:yes:"):
		code := NormalizeCompanyCode(strings.TrimPrefix(id, "link:yes:"))
		if code == "" {
			update("Something went wrong, run `/link` again.")
			return
		}
		// Look the company up again rather than trusting what was shown, in
		// case it changed or someone else linked it in the meantime.
		b.deferred(w, in, 6, func(ctx context.Context) botMessage { return botMessage{Content: b.link(ctx, user, code)} })
	default:
		b.deferred(w, in, 6, func(ctx context.Context) botMessage { return b.buttonPress(ctx, id, user) })
	}
}

// link is the second step of /link: link the user to the company.
func (b *Bot) link(ctx context.Context, user discordUser, code string) string {
	c, problem := b.lookupCompany(ctx, user, code)
	if problem != "" {
		return problem
	}
	u, err := b.store.LinkUser(ctx, user.ID, user.Username, user.Avatar, c)
	switch {
	case errors.Is(err, ErrCompanyTaken):
		return fmt.Sprintf("`%s` is already linked to another Discord account.", c.Code)
	case err != nil:
		log.Printf("link %s: %v", user.ID, err)
		return "Something went wrong, try again in a moment."
	}
	return fmt.Sprintf("**Linked** as **%s** ✅ You can use PrUn Forex now, and I'll DM you here when your orders get filled.\n%s", u.Name(), b.store.baseURL)
}

func reply(w http.ResponseWriter, in interaction, content string) {
	data := map[string]any{"content": content}
	if in.ephemeral() {
		data["flags"] = ephemeralFlag
	}
	writeJSON(w, map[string]any{"type": 4, "data": data})
}

// postMessage sends a message to a channel and returns its id.
func (b *Bot) postMessage(ctx context.Context, channelID, content string) (string, error) {
	var msg struct {
		ID string `json:"id"`
	}
	err := b.call(ctx, http.MethodPost, "/channels/"+channelID+"/messages", map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}, &msg)
	return msg.ID, err
}

func (b *Bot) editMessage(ctx context.Context, channelID, messageID, content string) error {
	return b.call(ctx, http.MethodPatch, "/channels/"+channelID+"/messages/"+messageID, map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}, nil)
}

func (b *Bot) deleteMessage(ctx context.Context, channelID, messageID string) error {
	return b.call(ctx, http.MethodDelete, "/channels/"+channelID+"/messages/"+messageID, nil, nil)
}

func (b *Bot) sendDM(ctx context.Context, discordID, content, components string) error {
	b.mu.Lock()
	ch := b.dmChannels[discordID]
	b.mu.Unlock()
	if ch == "" {
		var c struct {
			ID string `json:"id"`
		}
		if err := b.call(ctx, http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": discordID}, &c); err != nil {
			return err
		}
		ch = c.ID
		b.mu.Lock()
		b.dmChannels[discordID] = ch
		b.mu.Unlock()
	}
	msg := map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if components != "" {
		msg["components"] = json.RawMessage(components)
	}
	return b.call(ctx, http.MethodPost, "/channels/"+ch+"/messages", msg, nil)
}

const maxAttempts = 8

// RunOutbox delivers queued notifications until ctx is cancelled. Without a
// bot token it just logs them, which is handy for local dev.
func (b *Bot) RunOutbox(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pending, err := b.store.PendingNotifications(ctx, 20)
		if err != nil {
			log.Printf("outbox: %v", err)
			continue
		}
		for _, n := range pending {
			if err := b.deliver(ctx, n); err != nil {
				log.Printf("outbox: notification %d: %v", n.ID, err)
			}
		}
	}
}

func (b *Bot) deliver(ctx context.Context, n Notification) error {
	if b.token == "" || isDevID(n.DiscordID) {
		log.Printf("DM (not sent) to %s:\n%s", n.DiscordID, n.Message)
		return b.store.MarkSent(ctx, n.ID)
	}
	err := b.sendDM(ctx, n.DiscordID, n.Message, n.Components)
	if err == nil {
		return b.store.MarkSent(ctx, n.ID)
	}
	de, _ := err.(*discordError)
	switch {
	case de != nil && de.Code == errCannotDM:
		log.Printf("outbox: can't DM %s, unlinking", n.DiscordID)
		return b.store.MarkFailed(ctx, n, err, true)
	case de != nil && de.Status == http.StatusNotFound:
		// Stale DM channel; forget it and retry.
		b.mu.Lock()
		delete(b.dmChannels, n.DiscordID)
		b.mu.Unlock()
	}
	attempts := n.Attempts + 1
	if attempts >= maxAttempts || (de != nil && de.Status >= 400 && de.Status < 500 &&
		de.Status != http.StatusTooManyRequests && de.Status != http.StatusNotFound) {
		return b.store.MarkFailed(ctx, n, err, false)
	}
	delay := time.Duration(10<<attempts) * time.Second
	if de != nil && de.RetryAfter > 0 {
		delay = time.Duration(de.RetryAfter*float64(time.Second)) + time.Second
	}
	return b.store.MarkRetry(ctx, n.ID, attempts, delay, err)
}
