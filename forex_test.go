package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"), "https://forex.test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newUser(t *testing.T, s *Store, name string) *User {
	t.Helper()
	u, err := s.UpsertUser(context.Background(), "id-"+name, name, "", true)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustPlace(t *testing.T, s *Store, u *User, from, to string, amount int64, take bool) PlaceResult {
	t.Helper()
	res, err := s.PlaceOrder(context.Background(), u, from, to, amount, take, 0)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestFullMatch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")

	mustPlace(t, s, alice, "AIC", "NCC", 60, false)
	mustPlace(t, s, alice, "AIC", "NCC", 80, false)

	m, err := s.FindMatch(ctx, bob.ID, "NCC", "AIC", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Full() || m.Available != 100 || len(m.Legs) != 2 || m.Legs[0].Take != 60 || m.Legs[1].Take != 40 {
		t.Fatalf("unexpected match: %+v", m)
	}
	// Alice doesn't match against her own orders.
	if m, _ := s.FindMatch(ctx, alice.ID, "NCC", "AIC", 100); len(m.Legs) != 0 {
		t.Fatalf("matched own orders: %+v", m)
	}

	res := mustPlace(t, s, bob, "NCC", "AIC", 100, true)
	if res.Filled != 100 || res.Fills != 2 || res.Order != nil {
		t.Fatalf("unexpected result: %+v", res)
	}
	open, _ := s.ListOrders(ctx, OrderFilter{Status: "open"})
	if len(open) != 1 || open[0].Remaining != 40 {
		t.Fatalf("expected one order with 40 left, got %+v", open)
	}
	trades, _ := s.Trades(ctx, bob.ID, false, 10)
	if len(trades) != 2 || trades[0].Send != "NCC" || trades[0].Receive != "AIC" || trades[0].Counterparty != "alice" {
		t.Fatalf("unexpected trades for bob: %+v", trades)
	}
	pending, _ := s.PendingNotifications(ctx, 10)
	if len(pending) != 2 || pending[0].DiscordID != "id-alice" {
		t.Fatalf("expected 2 DMs for alice, got %+v", pending)
	}
}

func TestPartialMatchPostsRemainder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")

	mustPlace(t, s, alice, "AIC", "NCC", 100, false)
	m, _ := s.FindMatch(ctx, bob.ID, "NCC", "AIC", 150)
	if m.Full() || m.Available != 100 || m.Leftover() != 50 {
		t.Fatalf("unexpected match: %+v", m)
	}
	res := mustPlace(t, s, bob, "NCC", "AIC", 150, true)
	if res.Filled != 100 || res.Order == nil || res.Order.Amount != 50 {
		t.Fatalf("unexpected result: %+v", res)
	}
	o, _ := s.OrderByID(ctx, 1)
	if o.Status != "filled" || len(o.Fills) != 1 || o.Fills[0].Filler != "bob" {
		t.Fatalf("alice's order should be filled by bob: %+v", o)
	}
}

func TestIncreaseExistingOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")

	mustPlace(t, s, alice, "AIC", "NCC", 100, false)
	s.Fill(ctx, 1, bob, 30)
	if o, _ := s.ExistingOrder(ctx, alice.ID, "AIC", "NCC"); o == nil || o.ID != 1 {
		t.Fatalf("expected existing order #1, got %+v", o)
	}
	if o, _ := s.ExistingOrder(ctx, alice.ID, "NCC", "AIC"); o != nil {
		t.Fatalf("other direction shouldn't count: %+v", o)
	}

	// Bob has a counter-order; Alice takes it and adds the rest to #1.
	mustPlace(t, s, bob, "NCC", "AIC", 20, false)
	res, err := s.PlaceOrder(ctx, alice, "AIC", "NCC", 50, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Filled != 20 || !res.Increased || res.Order.ID != 1 || res.Order.Amount != 130 || res.Order.Remaining != 100 {
		t.Fatalf("unexpected result: %+v %+v", res, res.Order)
	}

	// Can't top up someone else's order, a closed order, or a different pair.
	if _, err := s.PlaceOrder(ctx, bob, "AIC", "NCC", 5, false, 1); !isUserError(err) {
		t.Fatalf("topping up another user's order: %v", err)
	}
	if _, err := s.PlaceOrder(ctx, alice, "CIS", "NCC", 5, false, 1); !isUserError(err) {
		t.Fatalf("topping up with a different pair: %v", err)
	}
	s.Cancel(ctx, 1, alice.ID)
	if _, err := s.PlaceOrder(ctx, alice, "AIC", "NCC", 5, false, 1); !isUserError(err) {
		t.Fatalf("topping up a cancelled order: %v", err)
	}
}

func TestSettleTrades(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob, carol := newUser(t, s, "alice"), newUser(t, s, "bob"), newUser(t, s, "carol")
	mustPlace(t, s, alice, "AIC", "NCC", 100, false)
	s.Fill(ctx, 1, bob, 30)
	s.Fill(ctx, 1, bob, 20)

	trades, _ := s.Trades(ctx, alice.ID, false, 10)
	if len(trades) != 2 {
		t.Fatalf("expected 2 trades, got %d", len(trades))
	}
	first := trades[0].FillID
	if err := s.SetSettled(ctx, first, carol.ID, true); !isUserError(err) {
		t.Fatalf("stranger settling a trade: %v", err)
	}
	if err := s.SetSettled(ctx, first, alice.ID, true); err != nil {
		t.Fatal(err)
	}

	// Hidden for alice, still visible for bob.
	if trades, _ := s.Trades(ctx, alice.ID, false, 10); len(trades) != 1 || trades[0].FillID == first {
		t.Fatalf("settled trade should be hidden for alice: %+v", trades)
	}
	if trades, _ := s.Trades(ctx, bob.ID, false, 10); len(trades) != 2 {
		t.Fatalf("bob's view shouldn't change: %+v", trades)
	}
	all, _ := s.Trades(ctx, alice.ID, true, 10)
	if len(all) != 2 || all[1].FillID != first || !all[1].Settled || all[0].Settled {
		t.Fatalf("show-settled should list it last, marked settled: %+v", all)
	}
	if n, _ := s.SettledCount(ctx, alice.ID); n != 1 {
		t.Fatalf("settled count for alice = %d", n)
	}
	if n, _ := s.SettledCount(ctx, bob.ID); n != 0 {
		t.Fatalf("settled count for bob = %d", n)
	}

	s.SetSettled(ctx, first, alice.ID, false)
	if trades, _ := s.Trades(ctx, alice.ID, false, 10); len(trades) != 2 {
		t.Fatalf("unsettling should bring it back: %+v", trades)
	}
}

func TestMigrateExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := OpenStore(path, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Reopening must not re-run migrations.
	s, err = OpenStore(path, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestKeepModeDoesNotFill(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	mustPlace(t, s, alice, "CIS", "ICA", 10, false)
	mustPlace(t, s, bob, "ICA", "CIS", 10, false)
	open, _ := s.ListOrders(ctx, OrderFilter{Status: "open"})
	if len(open) != 2 {
		t.Fatalf("expected both orders open, got %d", len(open))
	}
}

func TestFillRules(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	mustPlace(t, s, alice, "AIC", "CIS", 50, false)

	if _, err := s.Fill(ctx, 1, alice, 10); err != ErrOwnOrder {
		t.Fatalf("own order: got %v", err)
	}
	if _, err := s.Fill(ctx, 1, bob, 51); err != ErrTooMuch {
		t.Fatalf("overfill: got %v", err)
	}
	if n, err := s.Fill(ctx, 1, bob, 20); err != nil || n != 20 {
		t.Fatalf("partial: %d %v", n, err)
	}
	if n, err := s.Fill(ctx, 1, bob, 0); err != nil || n != 30 {
		t.Fatalf("rest: %d %v", n, err)
	}
	if _, err := s.Fill(ctx, 1, bob, 1); err != ErrClosed {
		t.Fatalf("filled order: got %v", err)
	}
	if err := s.Cancel(ctx, 1, alice.ID); err != ErrClosed {
		t.Fatalf("cancel filled: got %v", err)
	}
	if _, err := s.Fill(ctx, 99, bob, 1); err != ErrNotFound {
		t.Fatalf("missing: got %v", err)
	}
}

func TestConcurrentFillsNeverOverfill(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := newUser(t, s, "owner")
	mustPlace(t, s, owner, "AIC", "NCC", 100, false)

	const workers = 20
	fillers := make([]*User, workers)
	for i := range fillers {
		fillers[i] = newUser(t, s, "filler"+string(rune('a'+i)))
	}
	var filled atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(u *User) {
			defer wg.Done()
			// Half fill directly, half go through the "take" flow.
			if u.ID%2 == 0 {
				if n, err := s.Fill(ctx, 1, u, 15); err == nil {
					filled.Add(n)
				} else if err != ErrTooMuch && err != ErrClosed {
					t.Error(err)
				}
			} else {
				res, err := s.PlaceOrder(ctx, u, "NCC", "AIC", 15, true, 0)
				if err != nil {
					t.Error(err)
				}
				filled.Add(res.Filled)
			}
		}(fillers[i])
	}
	wg.Wait()

	o, err := s.OrderByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, f := range o.Fills {
		sum += f.Amount
	}
	if o.Remaining != 0 || o.Status != "filled" || sum != 100 || filled.Load() != 100 {
		t.Fatalf("remaining=%d status=%s fills=%d reported=%d", o.Remaining, o.Status, sum, filled.Load())
	}
}

func TestLinkStateAndUnreachableUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// Signing in doesn't link, /link does, signing in again keeps it.
	u, _ := s.UpsertUser(ctx, "123", "dave", "", false)
	if u.Linked {
		t.Fatal("new sign-in should not be linked")
	}
	s.UpsertUser(ctx, "123", "dave", "", true)
	if u, _ = s.UpsertUser(ctx, "123", "dave2", "", false); !u.Linked || u.Username != "dave2" {
		t.Fatalf("link lost on re-login: %+v", u)
	}

	bob := newUser(t, s, "bob")
	mustPlace(t, s, u, "AIC", "NCC", 10, false)
	mustPlace(t, s, u, "CIS", "NCC", 10, false)
	s.Fill(ctx, 1, bob, 5)
	s.Fill(ctx, 2, bob, 5)
	pending, _ := s.PendingNotifications(ctx, 10)
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(pending))
	}
	if err := s.MarkFailed(ctx, pending[0], ErrClosed, true); err != nil {
		t.Fatal(err)
	}
	if u, _ = s.UserByID(ctx, u.ID); u.Linked {
		t.Fatal("unreachable user should be unlinked")
	}
	if pending, _ = s.PendingNotifications(ctx, 10); len(pending) != 0 {
		t.Fatalf("remaining DMs should be dropped, got %d", len(pending))
	}
}

func TestInteractions(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := newTestStore(t)
	bot, err := NewBot("app", "", hex.EncodeToString(pub), s)
	if err != nil {
		t.Fatal(err)
	}
	send := func(body string, sign bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/discord/interactions", bytes.NewBufferString(body))
		ts := "1700000000"
		key := priv
		if !sign {
			_, key, _ = ed25519.GenerateKey(rand.Reader)
		}
		req.Header.Set("X-Signature-Timestamp", ts)
		req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(key, []byte(ts+body))))
		rec := httptest.NewRecorder()
		bot.HandleInteraction(rec, req)
		return rec
	}

	if rec := send(`{"type":1}`, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: got %d", rec.Code)
	}
	if rec := send(`{"type":1}`, true); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"type":1}` {
		t.Fatalf("ping: %d %s", rec.Code, rec.Body)
	}
	rec := send(`{"type":2,"data":{"name":"link"},"user":{"id":"555","username":"erin","global_name":"Erin"}}`, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Linked") {
		t.Fatalf("link: %d %s", rec.Code, rec.Body)
	}
	u, _ := s.UpsertUser(context.Background(), "555", "Erin", "", false)
	if !u.Linked {
		t.Fatal("user should be linked after /link")
	}
	send(`{"type":2,"data":{"name":"unlink"},"user":{"id":"555","username":"erin"}}`, true)
	if u, _ = s.UserByID(context.Background(), u.ID); u.Linked {
		t.Fatal("user should be unlinked after /unlink")
	}
}

func TestSessionCookie(t *testing.T) {
	srv := &Server{cfg: Config{SessionKey: []byte("0123456789abcdef0123456789abcdef")}}
	v := srv.sessionValue(42, timeNowPlus(1))
	if id, ok := srv.parseSession(v); !ok || id != 42 {
		t.Fatalf("valid session rejected: %v %v", id, ok)
	}
	if _, ok := srv.parseSession(strings.Replace(v, "42.", "43.", 1)); ok {
		t.Fatal("tampered session accepted")
	}
	if _, ok := srv.parseSession(srv.sessionValue(42, timeNowPlus(-1))); ok {
		t.Fatal("expired session accepted")
	}
}

func timeNowPlus(hours int) time.Time { return time.Now().Add(time.Duration(hours) * time.Hour) }
