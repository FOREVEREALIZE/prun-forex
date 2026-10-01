package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// A bot's status can only be set over Discord's gateway, so the bot holds a
// minimal connection: identify, heartbeat, and push "Watching 2.5m posted
// orders" whenever the open volume changes. No events are subscribed to.

const gatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"

type gatewayPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int            `json:"s"`
	T  string          `json:"t"`
}

// Gateway keeps the bot's presence up to date, reconnecting as needed.
type Gateway struct {
	token  string
	store  *Store
	url    string
	period time.Duration

	mu     sync.Mutex
	conn   *websocket.Conn
	status string
}

func NewGateway(token string, store *Store) *Gateway {
	return &Gateway{token: token, store: store, url: gatewayURL, period: time.Minute}
}

// Run connects and keeps reconnecting until ctx is done.
func (g *Gateway) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := g.session(ctx); err != nil && ctx.Err() == nil {
			log.Printf("gateway: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (g *Gateway) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, g.url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	g.mu.Lock()
	g.conn, g.status = conn, ""
	g.mu.Unlock()

	// HELLO carries the heartbeat interval.
	var hello gatewayPayload
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	var h struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	json.Unmarshal(hello.D, &h)
	if h.HeartbeatInterval <= 0 {
		h.HeartbeatInterval = 41250
	}

	presence, err := g.presence(ctx)
	if err != nil {
		return err
	}
	if err := g.send(gatewayPayload{Op: 2, D: mustJSON(map[string]any{
		"token":      g.token,
		"intents":    0,
		"properties": map[string]string{"os": "linux", "browser": "prun-forex", "device": "prun-forex"},
		"presence":   presence,
	})}); err != nil {
		return fmt.Errorf("identify: %w", err)
	}

	var seq struct {
		sync.Mutex
		n *int
	}
	go func() { // heartbeat
		t := time.NewTicker(time.Duration(h.HeartbeatInterval) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				seq.Lock()
				d := mustJSON(seq.n)
				seq.Unlock()
				if err := g.send(gatewayPayload{Op: 1, D: d}); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	go func() { // keep the status in step with the open volume
		t := time.NewTicker(g.period)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := g.UpdateStatus(ctx); err != nil {
					log.Printf("gateway: status: %v", err)
				}
			}
		}
	}()

	for {
		var p gatewayPayload
		if err := conn.ReadJSON(&p); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if p.S != nil {
			seq.Lock()
			seq.n = p.S
			seq.Unlock()
		}
		switch p.Op {
		case 1: // heartbeat requested
			seq.Lock()
			d := mustJSON(seq.n)
			seq.Unlock()
			if err := g.send(gatewayPayload{Op: 1, D: d}); err != nil {
				return err
			}
		case 7, 9: // reconnect, or invalid session
			return fmt.Errorf("asked to reconnect (op %d)", p.Op)
		}
	}
}

func (g *Gateway) send(p gatewayPayload) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn == nil {
		return fmt.Errorf("not connected")
	}
	return g.conn.WriteJSON(p)
}

// presence is the status to show: how much is up for trade right now.
func (g *Gateway) presence(ctx context.Context) (map[string]any, error) {
	volume, err := g.store.OpenVolume(ctx)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.status = statusText(volume)
	status := g.status
	g.mu.Unlock()
	return map[string]any{
		"since":      nil,
		"afk":        false,
		"status":     "online",
		"activities": []any{map[string]any{"name": status, "type": 3}}, // 3 = Watching
	}, nil
}

// UpdateStatus pushes the status, unless it hasn't changed.
func (g *Gateway) UpdateStatus(ctx context.Context) error {
	volume, err := g.store.OpenVolume(ctx)
	if err != nil {
		return err
	}
	g.mu.Lock()
	unchanged := g.status == statusText(volume)
	g.mu.Unlock()
	if unchanged {
		return nil
	}
	p, err := g.presence(ctx)
	if err != nil {
		return err
	}
	return g.send(gatewayPayload{Op: 3, D: mustJSON(p)})
}

// statusText reads as "2.5m posted orders" after Discord's "Watching".
func statusText(volume int64) string {
	return compactAmount(volume) + " posted orders"
}

// compactAmount shortens big numbers the way players write them: 2.5m, 350k.
func compactAmount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return trimZero(float64(n)/1e9) + "b"
	case n >= 1_000_000:
		return trimZero(float64(n)/1e6) + "m"
	case n >= 1_000:
		return trimZero(float64(n)/1e3) + "k"
	}
	return fmt.Sprint(n)
}

func trimZero(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	return strings.TrimSuffix(s, ".0")
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
