package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	code := strings.ToUpper(name)
	if len(code) > 4 {
		code = code[:4]
	}
	u, err := s.LinkUser(context.Background(), "id-"+name, name, "", Company{Code: code, UserName: strings.ToUpper(name)})
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
	if len(trades) != 2 || trades[0].Send != "NCC" || trades[0].Receive != "AIC" || trades[0].Counterparty.Handle != "alice" {
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
	if o.Status != "filled" || len(o.Fills) != 1 || o.Fills[0].Filler.Handle != "bob" {
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

func TestContFlow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob, carol := newUser(t, s, "alice"), newUser(t, s, "bob"), newUser(t, s, "carol")
	mustPlace(t, s, alice, "AIC", "NCC", 100, false)
	s.Fill(ctx, 1, bob, 30)
	s.Fill(ctx, 1, bob, 20)
	s.db.Exec(`UPDATE notifications SET sent_at = 1`) // ignore the fill DMs

	state := func(u *User) ContState {
		t.Helper()
		trades, err := s.Trades(ctx, u.ID, true, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range trades {
			if tr.FillID == 1 {
				return tr.Cont
			}
		}
		t.Fatalf("fill 1 not in %s's trades", u.Handle)
		return ""
	}
	act := func(u *User, a ContAction) error {
		t.Helper()
		_, err := s.ContAct(ctx, 1, u, a)
		return err
	}
	lastDM := func() Notification {
		t.Helper()
		pending, _ := s.PendingNotifications(ctx, 10)
		if len(pending) != 1 {
			t.Fatalf("expected exactly one queued DM, got %+v", pending)
		}
		s.MarkSent(ctx, pending[0].ID)
		return pending[0]
	}

	if st := state(alice); st != ContUndecided {
		t.Fatalf("new trade: %s", st)
	}
	if err := act(carol, ActRequest); err != errTradeNotFound {
		t.Fatalf("stranger: %v", err)
	}
	if err := act(alice, ActFulfill); !isUserError(err) {
		t.Fatalf("fulfill before CONT sent: %v", err)
	}

	// Alice asks Bob, Bob asks back, Alice accepts.
	if err := act(alice, ActRequest); err != nil {
		t.Fatal(err)
	}
	if dm := lastDM(); dm.UserID != bob.ID || !strings.Contains(dm.Message, "asks you to send the CONT") || !strings.Contains(dm.Message, "you provide **30 NCC**, they provide **30 AIC**") {
		t.Fatalf("request DM: %+v", dm)
	}
	if state(alice) != ContAskedThem || state(bob) != ContAskedMe {
		t.Fatalf("after request: alice=%s bob=%s", state(alice), state(bob))
	}
	if err := act(alice, ActRequest); err != errTradeChanged {
		t.Fatalf("asking twice: %v", err)
	}
	if err := act(bob, ActRequest); err != nil {
		t.Fatal(err)
	}
	if dm := lastDM(); dm.UserID != alice.ID || !strings.Contains(dm.Message, "instead") {
		t.Fatalf("counter-request DM: %+v", dm)
	}
	if state(alice) != ContAskedMe || state(bob) != ContAskedThem {
		t.Fatalf("after counter: alice=%s bob=%s", state(alice), state(bob))
	}
	if err := act(alice, ActSendMyself); err != nil {
		t.Fatal(err)
	}
	if dm := lastDM(); dm.UserID != bob.ID || !strings.Contains(dm.Message, "accepted") {
		t.Fatalf("accept DM: %+v", dm)
	}
	if state(alice) != ContMine || state(bob) != ContTheirs {
		t.Fatalf("after accept: alice=%s bob=%s", state(alice), state(bob))
	}

	// Only Alice can mark it sent, once.
	if err := act(bob, ActMarkSent); err != errTradeChanged {
		t.Fatalf("non-sender marking sent: %v", err)
	}
	if err := act(bob, ActSendMyself); err != errTradeChanged {
		t.Fatalf("volunteering after it's decided: %v", err)
	}
	if err := act(alice, ActMarkSent); err != nil {
		t.Fatal(err)
	}
	if dm := lastDM(); dm.UserID != bob.ID || !strings.Contains(dm.Message, "sent the CONT") {
		t.Fatalf("sent DM: %+v", dm)
	}
	if state(alice) != ContSentByMe || state(bob) != ContSentByThem {
		t.Fatalf("after sent: alice=%s bob=%s", state(alice), state(bob))
	}

	// Fulfilling hides it for that side only.
	if err := act(bob, ActFulfill); err != nil {
		t.Fatal(err)
	}
	if trades, _ := s.Trades(ctx, bob.ID, false, 10); len(trades) != 1 || trades[0].FillID == 1 {
		t.Fatalf("fulfilled trade should be hidden for bob: %+v", trades)
	}
	aliceTrades, _ := s.Trades(ctx, alice.ID, false, 10)
	if len(aliceTrades) != 2 || !aliceTrades[1].TheyFulfilled || aliceTrades[1].Fulfilled {
		t.Fatalf("alice should still see it, fulfilled by bob: %+v", aliceTrades)
	}
	all, _ := s.Trades(ctx, bob.ID, true, 10)
	if len(all) != 2 || all[1].FillID != 1 || !all[1].Fulfilled {
		t.Fatalf("show-fulfilled should list it last: %+v", all)
	}
	if n, _ := s.FulfilledCount(ctx, bob.ID); n != 1 {
		t.Fatalf("fulfilled count for bob = %d", n)
	}
	if n, _ := s.FulfilledCount(ctx, alice.ID); n != 0 {
		t.Fatalf("fulfilled count for alice = %d", n)
	}
	if err := act(bob, ActUnfulfill); err != nil {
		t.Fatal(err)
	}
	if trades, _ := s.Trades(ctx, bob.ID, false, 10); len(trades) != 2 {
		t.Fatalf("unfulfilling should bring it back: %+v", trades)
	}
	if pending, _ := s.PendingNotifications(ctx, 10); len(pending) != 0 {
		t.Fatalf("fulfilling shouldn't DM: %+v", pending)
	}
}

func TestMigrateSettledTrades(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	// A database from before the CONT workflow, with one trade settled.
	s := newTestStoreAt(t, path)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	mustPlace(t, s, alice, "AIC", "NCC", 10, false)
	s.Fill(ctx, 1, bob, 10)
	s.Close()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE fills DROP COLUMN contractor_id`,
		`ALTER TABLE fills DROP COLUMN cont_request_from`,
		`ALTER TABLE fills DROP COLUMN cont_sent_at`,
		`ALTER TABLE fills RENAME COLUMN owner_fulfilled_at TO owner_settled_at`,
		`ALTER TABLE fills RENAME COLUMN filler_fulfilled_at TO filler_settled_at`,
		`UPDATE fills SET owner_settled_at = 123`,
		`DROP INDEX users_company`,
		`ALTER TABLE users DROP COLUMN company_code`,
		`ALTER TABLE users DROP COLUMN company_user_name`,
		`ALTER TABLE users DROP COLUMN corp_code`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s = newTestStoreAt(t, path)
	trades, err := s.Trades(ctx, alice.ID, true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 || !trades[0].Fulfilled || trades[0].Cont != ContSent {
		t.Fatalf("settled trade should become fulfilled with a sent CONT: %+v", trades)
	}
	if trades, _ := s.Trades(ctx, bob.ID, false, 10); len(trades) != 1 || trades[0].Cont != ContSent {
		t.Fatalf("bob should still need to fulfill: %+v", trades)
	}
}

func newTestStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenStore(path, "https://forex.test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
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

func TestDMNumbersHaveSeparators(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	mustPlace(t, s, alice, "AIC", "NCC", 2_500_000, false)
	s.Fill(ctx, 1, bob, 1_234_567)
	pending, _ := s.PendingNotifications(ctx, 10)
	if len(pending) != 1 {
		t.Fatalf("expected one fill DM, got %+v", pending)
	}
	msg := pending[0].Message
	for _, want := range []string{
		"filled **1,234,567** of your",
		"**1,265,433 / 2,500,000** still open",
		"You provide **1,234,567 AIC**, they provide **1,234,567 NCC**",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("fill DM missing %q:\n%s", want, msg)
		}
	}
	s.MarkSent(ctx, pending[0].ID)

	if _, err := s.ContAct(ctx, 1, bob, ActRequest); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingNotifications(ctx, 10)
	if len(pending) != 1 || !strings.Contains(pending[0].Message, "you provide **1,234,567 AIC**, they provide **1,234,567 NCC**") {
		t.Fatalf("CONT DM: %+v", pending)
	}
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
		fillers[i] = newUser(t, s, "f"+string(rune('a'+i)))
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
	u, _ := s.UpsertUser(ctx, "123", "dave", "")
	if u.Linked || u.Ready() {
		t.Fatal("new sign-in should not be linked")
	}
	nd := Company{Code: "NDC", UserName: "Dave", CorpCode: "NE"}
	s.LinkUser(ctx, "123", "dave", "", nd)
	if u, _ = s.UpsertUser(ctx, "123", "dave2", ""); !u.Ready() || u.Handle != "dave2" || u.Name() != "[NE] Dave | NDC" {
		t.Fatalf("link lost on re-login: %+v", u)
	}
	// A company belongs to one Discord account.
	if _, err := s.LinkUser(ctx, "999", "mallory", "", nd); err != ErrCompanyTaken {
		t.Fatalf("second account claiming a company: %v", err)
	}
	if u, _ := s.LinkUser(ctx, "123", "dave2", "", Company{Code: "NDC", UserName: "Dave"}); u.Name() != "Dave | NDC" {
		t.Fatalf("relinking the same company, having left the corp: %q", u.Name())
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

func TestTraderName(t *testing.T) {
	for _, c := range []struct {
		tr   Trader
		want string
	}{
		{Trader{Handle: "nik", CompanyUser: "Nikuno", CompanyCode: "NIKU", CorpCode: "NE"}, "[NE] Nikuno | NIKU"},
		{Trader{Handle: "nik", CompanyUser: "Nikuno", CompanyCode: "NIKU"}, "Nikuno | NIKU"},
		{Trader{Handle: "nik"}, "nik"},
	} {
		if got := c.tr.Name(); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.tr, got, c.want)
		}
	}
}

func TestCompanyMigrationGatesLinkedUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	s := newTestStoreAt(t, path)
	s.UpsertUser(context.Background(), "1", "linked", "")
	s.UpsertUser(context.Background(), "2", "notlinked", "")
	s.Close()
	db, _ := sql.Open("sqlite", "file:"+path)
	for _, q := range []string{
		`DROP INDEX users_company`,
		`ALTER TABLE users DROP COLUMN company_code`,
		`ALTER TABLE users DROP COLUMN company_user_name`,
		`ALTER TABLE users DROP COLUMN corp_code`,
		`UPDATE users SET linked_at = 1 WHERE discord_id = '1'`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s = newTestStoreAt(t, path)
	if pending, _ := s.PendingNotifications(context.Background(), 10); len(pending) != 0 {
		t.Fatalf("migration shouldn't DM anyone: %+v", pending)
	}
	u, _ := s.UpsertUser(context.Background(), "1", "linked", "")
	if !u.Linked || u.Ready() {
		t.Fatalf("linked user without company should be gated: %+v", u)
	}
}

func TestFNARCompany(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/company/code/NIKU":
			w.Write([]byte(`{"UserName":"Nikuno","CompanyCode":"NIKU","CompanyId":"x","CorporationCode":"NE"}`))
		case "/company/code/SOLO":
			w.Write([]byte(`{"UserName":"Solo","CompanyCode":"SOLO","CorporationCode":null}`))
		case "/company/code/BOOM":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	f := NewFNAR(srv.URL)
	ctx := context.Background()
	if c, err := f.Company(ctx, "NIKU"); err != nil || c != (Company{Code: "NIKU", UserName: "Nikuno", CorpCode: "NE"}) {
		t.Fatalf("NIKU: %+v %v", c, err)
	}
	if c, err := f.Company(ctx, "SOLO"); err != nil || c.CorpCode != "" {
		t.Fatalf("SOLO: %+v %v", c, err)
	}
	if _, err := f.Company(ctx, "NOPE"); err != ErrNoCompany {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := f.Company(ctx, "BOOM"); err == nil || err == ErrNoCompany {
		t.Fatalf("server error should be a plain error: %v", err)
	}
	for in, want := range map[string]string{"niku": "NIKU", " ab1 ": "AB1", "TOOLONG": "", "a-b": "", "": ""} {
		if got := NormalizeCompanyCode(in); got != want {
			t.Errorf("NormalizeCompanyCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInteractions(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := newTestStore(t)

	// One fake server plays both FNAR and Discord's webhook API.
	type edit struct {
		Content    string
		Components []struct {
			Components []struct {
				Label    string `json:"label"`
				CustomID string `json:"custom_id"`
			} `json:"components"`
		}
	}
	var mu sync.Mutex
	var edits []edit
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/company/code/NIKU":
			w.Write([]byte(`{"UserName":"Nikuno","CompanyCode":"NIKU","CompanyName":"Nikuno Corp","CorporationCode":"NE","CorporationName":"Nikuno Enterprises"}`))
		case r.URL.Path == "/company/code/SOLO":
			w.Write([]byte(`{"UserName":"Solo","CompanyCode":"SOLO","CompanyName":"Solo Inc","CorporationCode":null}`))
		case strings.HasPrefix(r.URL.Path, "/company/code/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && r.URL.Path == "/webhooks/app/tok/messages/@original":
			var e edit
			json.NewDecoder(r.Body).Decode(&e)
			mu.Lock()
			edits = append(edits, e)
			mu.Unlock()
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fake.Close()
	bot, err := NewBot("app", "", hex.EncodeToString(pub), s, NewFNAR(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	bot.api = fake.URL

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
		bot.async.Wait()
		return rec
	}
	const erin = `"user":{"id":"555","username":"erin","global_name":"Erin"}`
	lastEdit := func() edit {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(edits) == 0 {
			t.Fatal("no deferred reply")
		}
		return edits[len(edits)-1]
	}
	link := func(code string) edit {
		t.Helper()
		rec := send(`{"type":2,"token":"tok","application_id":"app","data":{"name":"link","options":[{"name":"company_code","type":3,"value":"`+code+`"}]},`+erin+`}`, true)
		if rec.Code != 200 {
			t.Fatalf("link %s: %d %s", code, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), `"type":4`) {
			return edit{Content: rec.Body.String()} // answered right away
		}
		if !strings.Contains(rec.Body.String(), `"type":5`) {
			t.Fatalf("link %s should be deferred: %s", code, rec.Body)
		}
		return lastEdit()
	}
	press := func(customID string) (*httptest.ResponseRecorder, edit) {
		t.Helper()
		rec := send(`{"type":3,"token":"tok","application_id":"app","data":{"custom_id":"`+customID+`","component_type":2},`+erin+`}`, true)
		if strings.Contains(rec.Body.String(), `"type":6`) {
			return rec, lastEdit()
		}
		return rec, edit{}
	}
	linked := func() *User {
		t.Helper()
		u, _ := s.UpsertUser(context.Background(), "555", "erin", "")
		return u
	}

	if rec := send(`{"type":1}`, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: got %d", rec.Code)
	}
	if rec := send(`{"type":1}`, true); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"type":1}` {
		t.Fatalf("ping: %d %s", rec.Code, rec.Body)
	}

	if e := link("toolong"); !strings.Contains(e.Content, "company code") {
		t.Fatalf("invalid code: %s", e.Content)
	}
	if e := link("ZZZZ"); !strings.Contains(e.Content, "Couldn't find a company") || len(e.Components) != 0 {
		t.Fatalf("unknown company: %+v", e)
	}

	// Step 1 asks for confirmation and links nothing yet.
	e := link("niku")
	if e.Content != "You are **Nikuno** and own **Nikuno Corp** (`NIKU`), a part of **Nikuno Enterprises** (`NE`). Is this correct?" {
		t.Fatalf("confirm message: %q", e.Content)
	}
	if len(e.Components) != 1 || len(e.Components[0].Components) != 2 ||
		e.Components[0].Components[0].CustomID != "link:yes:NIKU" || e.Components[0].Components[1].CustomID != "link:no" {
		t.Fatalf("confirm buttons: %+v", e.Components)
	}
	if linked().Linked {
		t.Fatal("shouldn't link before confirming")
	}
	if e := link("solo"); e.Content != "You are **Solo** and own **Solo Inc** (`SOLO`). Is this correct?" {
		t.Fatalf("confirm without corp: %q", e.Content)
	}

	// No: nothing linked, buttons removed.
	if rec, _ := press("link:no"); !strings.Contains(rec.Body.String(), `"type":7`) ||
		!strings.Contains(rec.Body.String(), "nothing was linked") || !strings.Contains(rec.Body.String(), `"components":[]`) {
		t.Fatalf("no: %s", rec.Body)
	}
	if linked().Linked {
		t.Fatal("No shouldn't link")
	}

	// Yes: linked, buttons removed.
	_, e = press("link:yes:NIKU")
	if !strings.Contains(e.Content, "Linked") || !strings.Contains(e.Content, "[NE] Nikuno | NIKU") || e.Components == nil || len(e.Components) != 0 {
		t.Fatalf("yes: %+v", e)
	}
	if u := linked(); !u.Ready() || u.Handle != "erin" || u.CompanyCode != "NIKU" {
		t.Fatalf("user should be linked with company after Yes: %+v", u)
	}

	// Someone else can't get to the confirm step for a taken company.
	s.LinkUser(context.Background(), "777", "other", "", Company{Code: "SOLO", UserName: "Solo"})
	if e := link("solo"); !strings.Contains(e.Content, "already linked") || len(e.Components) != 0 {
		t.Fatalf("taken company: %+v", e)
	}

	send(`{"type":2,"data":{"name":"unlink"},`+erin+`}`, true)
	if linked().Linked {
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
