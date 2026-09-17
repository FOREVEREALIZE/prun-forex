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

// RegisterCommands replaces the app's global commands with /link and /unlink,
// usable in a DM with the bot, whether the app is installed on a user or a server.
func (b *Bot) RegisterCommands(ctx context.Context) error {
	cmd := func(name, desc string, options ...any) map[string]any {
		return map[string]any{
			"name": name, "description": desc, "type": 1, "options": options,
			"integration_types": []int{0, 1}, // guild install, user install
			"contexts":          []int{1},    // bot DM
		}
	}
	companyCode := map[string]any{
		"type": 3, "name": "company_code", "required": true, "min_length": 1, "max_length": 4,
		"description": "Your Prosperous Universe company code, e.g. NIKU",
	}
	return b.call(ctx, http.MethodPut, "/applications/"+b.appID+"/commands", []any{
		cmd("link", "Link your Discord account and PrUn company so you can trade and get fill DMs", companyCode),
		cmd("unlink", "Stop PrUn Forex from DMing you (you'll need to /link again to trade)"),
	}, nil)
}

type interaction struct {
	Type          int    `json:"type"`
	Token         string `json:"token"`
	ApplicationID string `json:"application_id"`
	Data          struct {
		Name     string `json:"name"`
		CustomID string `json:"custom_id"`
		Options  []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"options"`
	} `json:"data"`
	User   *discordUser `json:"user"`
	Member *struct {
		User *discordUser `json:"user"`
	} `json:"member"`
}

func (in interaction) option(name string) string {
	for _, o := range in.Data.Options {
		if o.Name == name {
			var v string
			json.Unmarshal(o.Value, &v)
			return v
		}
	}
	return ""
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
		reply(w, "Couldn't tell who you are.")
		return
	}
	if in.Type == 3 {
		b.handleButton(w, in, *user)
		return
	}
	ctx := r.Context()
	switch in.Data.Name {
	case "link":
		code := NormalizeCompanyCode(in.option("company_code"))
		if code == "" {
			reply(w, "Give your company code (1–4 letters), e.g. `/link company_code:NIKU`.")
			return
		}
		b.deferred(w, in, 5, func(ctx context.Context) botMessage { return b.confirmLink(ctx, *user, code) })
	case "unlink":
		if err := b.store.Unlink(ctx, user.ID); err != nil {
			log.Printf("unlink %s: %v", user.ID, err)
			reply(w, "Something went wrong, try again in a moment.")
			return
		}
		reply(w, "Unlinked. I won't DM you anymore. Run `/link company_code:ABCD` to trade again.")
	default:
		reply(w, "Unknown command.")
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
	writeJSON(w, map[string]any{"type": ackType})
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
		update("That button doesn't do anything anymore. Run `/link` again.")
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

func reply(w http.ResponseWriter, content string) {
	writeJSON(w, map[string]any{"type": 4, "data": map[string]any{"content": content}})
}

func (b *Bot) sendDM(ctx context.Context, discordID, content string) error {
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
	return b.call(ctx, http.MethodPost, "/channels/"+ch+"/messages", map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}, nil)
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
	err := b.sendDM(ctx, n.DiscordID, n.Message)
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
