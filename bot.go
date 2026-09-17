package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
	http   *http.Client

	mu         sync.Mutex
	dmChannels map[string]string // discord user id -> DM channel id
}

func NewBot(appID, token, publicKeyHex string, store *Store) (*Bot, error) {
	b := &Bot{appID: appID, token: token, store: store,
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
	req, err := http.NewRequestWithContext(ctx, method, discordAPI+path, rd)
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
	cmd := func(name, desc string) map[string]any {
		return map[string]any{
			"name": name, "description": desc, "type": 1,
			"integration_types": []int{0, 1}, // guild install, user install
			"contexts":          []int{1},    // bot DM
		}
	}
	return b.call(ctx, http.MethodPut, "/applications/"+b.appID+"/commands", []any{
		cmd("link", "Link your Discord account to PrUn Forex so you can trade and get fill DMs"),
		cmd("unlink", "Stop PrUn Forex from DMing you (you'll need to /link again to trade)"),
	}, nil)
}

type interaction struct {
	Type int `json:"type"`
	Data struct {
		Name string `json:"name"`
	} `json:"data"`
	User   *discordUser `json:"user"`
	Member *struct {
		User *discordUser `json:"user"`
	} `json:"member"`
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
	case 2: // APPLICATION_COMMAND
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
	ctx := r.Context()
	switch in.Data.Name {
	case "link":
		if _, err := b.store.UpsertUser(ctx, user.ID, user.displayName(), user.Avatar, true); err != nil {
			log.Printf("link %s: %v", user.ID, err)
			reply(w, "Something went wrong, try again in a moment.")
			return
		}
		reply(w, "**Linked** ✅ You can use PrUn Forex now, and I'll DM you here when your orders get filled.\n"+b.store.baseURL)
	case "unlink":
		if err := b.store.Unlink(ctx, user.ID); err != nil {
			log.Printf("unlink %s: %v", user.ID, err)
			reply(w, "Something went wrong, try again in a moment.")
			return
		}
		reply(w, "Unlinked. I won't DM you anymore. Run `/link` to trade again.")
	default:
		reply(w, "Unknown command.")
	}
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
