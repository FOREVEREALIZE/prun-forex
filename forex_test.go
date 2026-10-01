package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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
		`ALTER TABLE notifications DROP COLUMN components`,
		`ALTER TABLE fills DROP COLUMN cancelled_at`,
		`ALTER TABLE fills DROP COLUMN cancelled_by`,
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

func TestCancelTrade(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob, carol := newUser(t, s, "alice"), newUser(t, s, "bob"), newUser(t, s, "carol")
	mustPlace(t, s, alice, "AIC", "NCC", 100, false)
	s.Fill(ctx, 1, bob, 60)
	s.Fill(ctx, 1, bob, 40)
	if o, _ := s.OrderByID(ctx, 1); o.Remaining != 0 || o.Status != "filled" {
		t.Fatalf("order should be filled: %+v", o)
	}
	s.db.Exec(`UPDATE notifications SET sent_at = 1`) // ignore the fill DMs

	if _, err := s.ContAct(ctx, 1, carol, ActCancel); err != errTradeNotFound {
		t.Fatalf("stranger calling off a trade: %v", err)
	}

	// The filler calls the first one off: it's un-filled and alice is told.
	if _, err := s.ContAct(ctx, 1, bob, ActCancel); err != nil {
		t.Fatal(err)
	}
	o, _ := s.OrderByID(ctx, 1)
	if o.Remaining != 60 || o.Status != "open" || len(o.Fills) != 1 || o.Fills[0].ID != 2 {
		t.Fatalf("order should reopen with 60 and one fill left: %+v", o)
	}
	pending, _ := s.PendingNotifications(ctx, 10)
	if len(pending) != 1 || pending[0].UserID != alice.ID ||
		!strings.Contains(pending[0].Message, "called off") ||
		!strings.Contains(pending[0].Message, "has **60 AIC** open again") {
		t.Fatalf("cancel DM: %+v", pending)
	}
	s.MarkSent(ctx, pending[0].ID)

	// It's gone from both sides' trades, and can't be acted on again.
	for _, u := range []*User{alice, bob} {
		trades, _ := s.Trades(ctx, u.ID, true, 10)
		if len(trades) != 1 || trades[0].FillID != 2 {
			t.Fatalf("%s should only see the other trade: %+v", u.Handle, trades)
		}
	}
	for _, a := range []ContAction{ActCancel, ActSendMyself, ActRequest} {
		if _, err := s.ContAct(ctx, 1, bob, a); !isUserError(err) {
			t.Fatalf("%s on a called-off trade: %v", a, err)
		}
	}

	// The owner can call one off too, but not once the CONT is sent.
	if _, err := s.ContAct(ctx, 2, alice, ActSendMyself); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ContAct(ctx, 2, alice, ActMarkSent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ContAct(ctx, 2, alice, ActCancel); !isUserError(err) {
		t.Fatalf("calling off after the CONT is sent: %v", err)
	}
	if o, _ := s.OrderByID(ctx, 1); o.Remaining != 60 {
		t.Fatalf("failed cancel shouldn't change the order: %+v", o)
	}
}

func TestCancelTradeByOwnerReopensCancelledOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	mustPlace(t, s, alice, "AIC", "NCC", 50, false)
	s.Fill(ctx, 1, bob, 20)
	s.Cancel(ctx, 1, alice.ID)

	if _, err := s.ContAct(ctx, 1, alice, ActCancel); err != nil {
		t.Fatal(err)
	}
	o, _ := s.OrderByID(ctx, 1)
	if o.Status != "cancelled" || o.Remaining != 50 {
		t.Fatalf("a cancelled order stays cancelled: %+v", o)
	}
	pending, _ := s.PendingNotifications(ctx, 10)
	if len(pending) == 0 || pending[len(pending)-1].UserID != bob.ID ||
		strings.Contains(pending[len(pending)-1].Message, "open again") {
		t.Fatalf("the filler is told, without the order line: %+v", pending)
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

	if _, _, err := s.Fill(ctx, 1, alice, 10); err != ErrOwnOrder {
		t.Fatalf("own order: got %v", err)
	}
	if _, _, err := s.Fill(ctx, 1, bob, 51); err != ErrTooMuch {
		t.Fatalf("overfill: got %v", err)
	}
	if n, _, err := s.Fill(ctx, 1, bob, 20); err != nil || n != 20 {
		t.Fatalf("partial: %d %v", n, err)
	}
	if n, _, err := s.Fill(ctx, 1, bob, 0); err != nil || n != 30 {
		t.Fatalf("rest: %d %v", n, err)
	}
	if _, _, err := s.Fill(ctx, 1, bob, 1); err != ErrClosed {
		t.Fatalf("filled order: got %v", err)
	}
	if err := s.Cancel(ctx, 1, alice.ID); err != ErrClosed {
		t.Fatalf("cancel filled: got %v", err)
	}
	if _, _, err := s.Fill(ctx, 99, bob, 1); err != ErrNotFound {
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
				if n, _, err := s.Fill(ctx, 1, u, 15); err == nil {
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
		`ALTER TABLE notifications DROP COLUMN components`,
		`ALTER TABLE fills DROP COLUMN cancelled_at`,
		`ALTER TABLE fills DROP COLUMN cancelled_by`,
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

// discordHarness drives the bot through its interactions endpoint, the way
// Discord would, capturing the messages it sends back.
type discordHarness struct {
	t        *testing.T
	bot      *Bot
	store    *Store
	priv     ed25519.PrivateKey
	mu       sync.Mutex
	edits    []botReply
	fnarBody string
	lastAck  string // the immediate response Discord got
	channel  *channelLog
}

// hidden reports whether the last reply was ephemeral (only the caller sees it).
func (h *discordHarness) hidden() bool {
	return strings.Contains(h.lastAck, `"flags":64`)
}

type botReply struct {
	Content    string `json:"content"`
	Components []struct {
		Components []struct {
			Label    string `json:"label"`
			CustomID string `json:"custom_id"`
		} `json:"components"`
	} `json:"components"`
}

func (r botReply) buttons() map[string]string {
	out := map[string]string{}
	for _, row := range r.Components {
		for _, b := range row.Components {
			out[b.Label] = b.CustomID
		}
	}
	return out
}

func newDiscordHarness(t *testing.T, s *Store) *discordHarness {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h := &discordHarness{t: t, store: s, priv: priv,
		fnarBody: `{"UserName":"Nikuno","CompanyCode":"NIKU","CompanyName":"Nikuno Corp","CorporationCode":"NE","CorporationName":"Nikuno Enterprises"}`}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/company/code/"):
			w.Write([]byte(h.fnarBody))
		case strings.HasPrefix(r.URL.Path, "/channels/") && h.channel != nil:
			h.channel.serve(w, r)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/messages/@original"):
			var e botReply
			json.NewDecoder(r.Body).Decode(&e)
			h.mu.Lock()
			h.edits = append(h.edits, e)
			h.mu.Unlock()
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(fake.Close)
	bot, err := NewBot("app", "", hex.EncodeToString(pub), s, NewFNAR(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	bot.api = fake.URL
	h.bot = bot
	return h
}

// send posts a signed interaction and returns the message the bot ended up with.
func (h *discordHarness) send(u *User, body string) botReply {
	h.t.Helper()
	body = strings.ReplaceAll(body, "@USER", `"user":{"id":"`+u.DiscordID+`","username":"`+u.Handle+`"}`)
	req := httptest.NewRequest(http.MethodPost, "/discord/interactions", bytes.NewBufferString(body))
	ts := "1700000000"
	req.Header.Set("X-Signature-Timestamp", ts)
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(h.priv, []byte(ts+body))))
	rec := httptest.NewRecorder()
	h.bot.HandleInteraction(rec, req)
	h.bot.async.Wait()
	if rec.Code != 200 {
		h.t.Fatalf("interaction failed: %d %s", rec.Code, rec.Body)
	}
	h.lastAck = rec.Body.String()
	if !strings.Contains(rec.Body.String(), `"type":5`) && !strings.Contains(rec.Body.String(), `"type":6`) {
		var immediate struct {
			Data botReply `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &immediate)
		return immediate.Data
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.edits) == 0 {
		h.t.Fatal("no deferred reply")
	}
	return h.edits[len(h.edits)-1]
}

func (h *discordHarness) cmd(u *User, name string, options string) botReply {
	h.t.Helper()
	return h.cmdIn(u, 1, name, options) // the bot's own DM
}

// cmdIn runs a command in a context: 0 a server, 1 the bot's DM, 2 another DM.
func (h *discordHarness) cmdIn(u *User, context int, name, options string) botReply {
	h.t.Helper()
	return h.send(u, `{"type":2,"token":"tok","application_id":"app","context":`+strconv.Itoa(context)+
		`,"data":{"name":"`+name+`","options":[`+options+`]},@USER}`)
}

func (h *discordHarness) press(u *User, customID string) botReply {
	h.t.Helper()
	return h.send(u, `{"type":3,"token":"tok","application_id":"app","data":{"custom_id":"`+customID+`","component_type":2},@USER}`)
}

func str(name, v string) string { return `{"name":"` + name + `","type":3,"value":"` + v + `"}` }
func num(name string, v int) string {
	return `{"name":"` + name + `","type":4,"value":` + strconv.Itoa(v) + `}`
}

func TestDiscordCommands(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	h := newDiscordHarness(t, s)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")

	// Someone who hasn't linked is told to.
	stranger, _ := s.UpsertUser(ctx, "nope", "nope", "")
	if r := h.cmd(stranger, "orders", ""); !strings.Contains(r.Content, "/link") {
		t.Fatalf("unlinked user: %q", r.Content)
	}

	// Posting, with nothing to match.
	r := h.cmd(alice, "post", num("amount", 100)+","+str("from", "AIC")+","+str("to", "NCC"))
	if !strings.Contains(r.Content, "Order `#1` posted") || r.buttons()["Cancel #1"] != "order:cancel:1" {
		t.Fatalf("post: %q %v", r.Content, r.buttons())
	}

	// Listing shows it; bob can fill it.
	if r := h.cmd(bob, "orders", ""); !strings.Contains(r.Content, "```ansi") ||
		!strings.Contains(r.Content, "ALICE (ALIC)") || !strings.Contains(r.Content, "\x1b[0;32m100\x1b[0;0m / 100") {
		t.Fatalf("orders: %q", r.Content)
	}
	if r := h.cmd(bob, "orders", `{"name":"mobile","type":5,"value":true}`); strings.Contains(r.Content, "```") ||
		!strings.Contains(r.Content, "`#1` **AIC → NCC** — **100** open of 100 — ALICE | ALIC") {
		t.Fatalf("orders mobile: %q", r.Content)
	}
	if r := h.cmd(bob, "orders", str("from", "CIS")); !strings.Contains(r.Content, "No open orders") {
		t.Fatalf("filtered orders: %q", r.Content)
	}
	r = h.cmd(bob, "fill", num("order", 1)+","+num("amount", 40))
	if !strings.Contains(r.Content, "Filled **40** on order `#1`") || !strings.Contains(r.Content, "who sends the CONT?") {
		t.Fatalf("fill: %q", r.Content)
	}
	if r.buttons()["I'll send it"] != "cont:self:1" || r.buttons()["Call off"] != "cont:cancel:1" {
		t.Fatalf("fill buttons: %v", r.buttons())
	}

	// Alice's fill DM carries the same buttons.
	pending, _ := s.PendingNotifications(ctx, 10)
	if len(pending) != 1 || !strings.Contains(pending[0].Components, `"cont:self:1"`) {
		t.Fatalf("fill DM buttons: %+v", pending)
	}
	s.MarkSent(ctx, pending[0].ID)

	// The whole CONT dance over buttons.
	if r := h.press(alice, "cont:request:1"); !strings.Contains(r.Content, "you asked BOB to send the CONT") {
		t.Fatalf("request: %q", r.Content)
	}
	if r := h.press(bob, "cont:self:1"); !strings.Contains(r.Content, "**you send the CONT**") || r.buttons()["CONT sent"] != "cont:sent:1" {
		t.Fatalf("accept: %q %v", r.Content, r.buttons())
	}
	if r := h.press(alice, "cont:sent:1"); !strings.Contains(r.Content, "BOB sends the CONT") {
		t.Fatalf("non-sender marking sent should say so: %q", r.Content)
	}
	if r := h.press(bob, "cont:sent:1"); !strings.Contains(r.Content, "you sent the CONT") || r.buttons()["Mark fulfilled"] != "cont:fulfill:1" {
		t.Fatalf("sent: %q %v", r.Content, r.buttons())
	}
	if r := h.press(bob, "cont:fulfill:1"); !strings.Contains(r.Content, "fulfilled by you") || r.buttons()["Undo"] != "cont:unfulfill:1" {
		t.Fatalf("fulfill: %q %v", r.Content, r.buttons())
	}
	if r := h.cmd(bob, "trades", ""); !strings.Contains(r.Content, "Nothing left to settle") {
		t.Fatalf("trades after fulfilling: %q", r.Content)
	}
	if r := h.cmd(bob, "trades", `{"name":"fulfilled","type":5,"value":true}`); !strings.Contains(r.Content, "fulfilled by you") ||
		r.buttons()["Trade #1"] != "trade:1" {
		t.Fatalf("trades with fulfilled: %q %v", r.Content, r.buttons())
	}
	if r := h.press(bob, "trade:1"); !strings.Contains(r.Content, "order `#1`") {
		t.Fatalf("trade button: %q", r.Content)
	}

	// Orders of your own, and cancelling.
	if r := h.cmd(alice, "myorders", ""); !strings.Contains(r.Content, "`#1` **AIC → NCC** — 40 of 100 filled — open") {
		t.Fatalf("myorders: %q", r.Content)
	}
	if r := h.press(bob, "order:cancel:1"); !strings.Contains(r.Content, "Couldn't cancel") {
		t.Fatalf("cancelling someone else's order: %q", r.Content)
	}
	if r := h.press(alice, "order:cancel:1"); !strings.Contains(r.Content, "cancelled") {
		t.Fatalf("cancel: %q", r.Content)
	}
	if o, _ := s.OrderByID(ctx, 1); o.Status != "cancelled" {
		t.Fatalf("order should be cancelled: %+v", o)
	}
}

func TestDiscordPostWithMatchAndExistingOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	h := newDiscordHarness(t, s)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	mustPlace(t, s, alice, "AIC", "NCC", 100, false)

	// Bob's order could be filled by Alice's: he's asked first.
	r := h.cmd(bob, "post", num("amount", 150)+","+str("from", "NCC")+","+str("to", "AIC"))
	if !strings.Contains(r.Content, "cover **100** of your **150 NCC**, leaving **50**") {
		t.Fatalf("match prompt: %q", r.Content)
	}
	take := r.buttons()["Fill 100, post the rest"]
	if take != "post:take:NCC:AIC:150:0" || r.buttons()["Post it anyway"] != "post:keep:NCC:AIC:150:0" {
		t.Fatalf("match buttons: %v", r.buttons())
	}
	if r := h.press(bob, take); !strings.Contains(r.Content, "Filled **100 NCC → AIC**") || !strings.Contains(r.Content, "remaining **50**") {
		t.Fatalf("take: %q", r.Content)
	}
	if o, _ := s.OrderByID(ctx, 1); o.Status != "filled" {
		t.Fatalf("alice's order should be filled: %+v", o)
	}

	// Posting the same pair again asks whether to add to the order he has.
	r = h.cmd(bob, "post", num("amount", 25)+","+str("from", "NCC")+","+str("to", "AIC"))
	if !strings.Contains(r.Content, "already have an open **NCC → AIC** order `#2`") {
		t.Fatalf("dup prompt: %q", r.Content)
	}
	add := r.buttons()["Add to #2"]
	if add != "post:post:NCC:AIC:25:2" || r.buttons()["Post separately"] != "post:post:NCC:AIC:25:0" {
		t.Fatalf("dup buttons: %v", r.buttons())
	}
	if r := h.press(bob, add); !strings.Contains(r.Content, "Added **25** to order `#2`, now **75** open") {
		t.Fatalf("add to existing: %q", r.Content)
	}
	if o, _ := s.OrderByID(ctx, 2); o.Amount != 75 {
		t.Fatalf("order should have grown: %+v", o)
	}

	// Calling a trade off from a button puts the amount back.
	trades, _ := s.Trades(ctx, alice.ID, false, 5)
	if r := h.press(alice, fmt.Sprintf("cont:cancel:%d", trades[0].FillID)); !strings.Contains(r.Content, "called off") {
		t.Fatalf("call off: %q", r.Content)
	}
	if o, _ := s.OrderByID(ctx, 1); o.Status != "open" || o.Remaining != 100 {
		t.Fatalf("order should reopen: %+v", o)
	}
}

func TestDiscordWorksOutsideBotDM(t *testing.T) {
	s := newTestStore(t)
	h := newDiscordHarness(t, s)
	alice := newUser(t, s, "alice")
	mustPlace(t, s, alice, "AIC", "NCC", 100, false)

	// Every command is offered in servers and other DMs, not just the bot's.
	for _, c := range h.bot.commands() {
		cmd := c.(map[string]any)
		contexts, installs := "[0 1 2]", "[0 1]"
		switch cmd["name"] {
		case "link":
			contexts = "[1]" // linking stays in the bot's DM
		case "board":
			contexts, installs = "[0]", "[0]" // boards belong to a server
		}
		if got := cmd["contexts"]; fmt.Sprint(got) != contexts {
			t.Errorf("/%s contexts = %v, want %s", cmd["name"], got, contexts)
		}
		if got := cmd["integration_types"]; fmt.Sprint(got) != installs {
			t.Errorf("/%s integration_types = %v", cmd["name"], got)
		}
	}

	// The board is public, like it is on the site.
	if r := h.cmdIn(alice, 0, "orders", ""); h.hidden() || !strings.Contains(r.Content, "#1") {
		t.Fatalf("orders in a server should be visible: hidden=%v %q", h.hidden(), r.Content)
	}
	if h.cmdIn(alice, 0, "orders", `{"name":"private","type":5,"value":true}`); !h.hidden() {
		t.Fatal("orders with private:True should be hidden")
	}

	// /link only works in the bot's DM, wherever it's somehow invoked from.
	if r := h.cmdIn(alice, 0, "link", str("company_code", "NIKU")); !strings.Contains(r.Content, "in a DM with me") {
		t.Fatalf("/link in a server: %q", r.Content)
	}
	if u, _ := s.UserByID(context.Background(), alice.ID); u.CompanyCode != "ALIC" {
		t.Fatalf("/link outside a DM shouldn't link: %+v", u)
	}

	// Anything personal is shown only to whoever ran it.
	for _, name := range []string{"myorders", "trades", "post", "fill", "cancel", "trade"} {
		h.cmdIn(alice, 0, name, "")
		if !h.hidden() {
			t.Errorf("/%s in a server should be hidden", name)
		}
		h.cmdIn(alice, 2, name, "")
		if !h.hidden() {
			t.Errorf("/%s in another DM should be hidden", name)
		}
	}

	// In the bot's own DM nothing is hidden, so the messages stay in history.
	for _, name := range []string{"orders", "myorders", "trades"} {
		if h.cmd(alice, name, ""); h.hidden() {
			t.Errorf("/%s in the bot DM shouldn't be hidden", name)
		}
	}

	// A command from a server member comes as member.user rather than user.
	body := `{"type":2,"token":"tok","application_id":"app","context":0,"data":{"name":"myorders","options":[]},` +
		`"member":{"user":{"id":"` + alice.DiscordID + `","username":"alice"}}}`
	if r := h.send(alice, body); !strings.Contains(r.Content, "Your orders:") {
		t.Fatalf("member command: %q", r.Content)
	}
}

// channelLog stands in for a Discord channel, recording what the bot does to it.
type channelLog struct {
	mu      sync.Mutex
	actions []string // "post", "edit" or "delete", in order
	posts   []string // contents of posts and edits, newest last
	nextID  int
	fail    bool // refuse everything, like a channel the bot can't write to
}

func (c *channelLog) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"code":50013,"message":"Missing Permissions"}`))
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == http.MethodDelete:
		c.actions = append(c.actions, "delete")
	case r.Method == http.MethodPatch:
		c.actions = append(c.actions, "edit")
		c.posts = append(c.posts, body.Content)
	default:
		c.nextID++
		c.actions = append(c.actions, "post")
		c.posts = append(c.posts, body.Content)
	}
	fmt.Fprintf(w, `{"id":"m%d"}`, c.nextID)
}

func (c *channelLog) actionList() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.actions, ",")
}

func (c *channelLog) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.posts) == 0 {
		return ""
	}
	return c.posts[len(c.posts)-1]
}

func (c *channelLog) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions, c.posts = nil, nil
}

func (h *discordHarness) withChannel(c *channelLog) { h.channel = c }

func TestDiscordBoard(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	ch := &channelLog{}
	h := newDiscordHarness(t, s)
	h.withChannel(ch)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	mustPlace(t, s, alice, "AIC", "NCC", 100, false)

	setup := func(opts string) botReply {
		t.Helper()
		return h.send(alice, `{"type":2,"token":"tok","application_id":"app","context":0,"guild_id":"g1",`+
			`"data":{"name":"board","options":[{"name":"setup","type":1,"options":[`+opts+`]}]},`+
			`"member":{"permissions":"32","user":{"id":"`+alice.DiscordID+`","username":"alice"}}}`)
	}
	channelOpt := `{"name":"channel","type":7,"value":"c1"}`

	// Someone without Manage Server is turned away.
	noPerms := h.send(alice, `{"type":2,"token":"tok","application_id":"app","context":0,"guild_id":"g1",`+
		`"data":{"name":"board","options":[{"name":"list","type":1,"options":[]}]},`+
		`"member":{"permissions":"0","user":{"id":"`+alice.DiscordID+`","username":"alice"}}}`)
	if !strings.Contains(noPerms.Content, "Manage Server") {
		t.Fatalf("without permission: %q", noPerms.Content)
	}

	// Setting up posts the board straight away.
	if r := setup(channelOpt + `,{"name":"mode","type":3,"value":"live"}`); !strings.Contains(r.Content, "when orders change") {
		t.Fatalf("setup: %q", r.Content)
	}
	if got := ch.actionList(); got != "post" {
		t.Fatalf("setup should post once, did %q", got)
	}
	if posted := ch.last(); !strings.Contains(posted, "```ansi") || !strings.Contains(posted, "ALICE (ALIC)") {
		t.Fatalf("board content: %q", posted)
	}

	boards, _ := s.Boards(ctx, "g1")
	if len(boards) != 1 {
		t.Fatalf("expected one board, got %+v", boards)
	}
	refresh := func() {
		t.Helper()
		boards, _ := s.Boards(ctx, "g1")
		for _, b := range boards {
			if err := h.bot.refreshBoard(ctx, b); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Nothing changed: the board is left alone.
	ch.reset()
	refresh()
	if got := ch.actionList(); got != "" {
		t.Fatalf("unchanged board shouldn't be touched, did %q", got)
	}

	// A fill is edited into the message where it is.
	s.Fill(ctx, 1, bob, 40)
	refresh()
	if got := ch.actionList(); got != "edit" {
		t.Fatalf("a fill should be edited in, did %q", got)
	}
	if !strings.Contains(ch.last(), "60") {
		t.Fatalf("edited board should show 60 open: %q", ch.last())
	}

	// A new order reposts, so it lands at the bottom of the channel.
	ch.reset()
	mustPlace(t, s, bob, "CIS", "ICA", 7, false)
	refresh()
	if got := ch.actionList(); got != "post,delete" {
		t.Fatalf("a new order should repost and remove the old, did %q", got)
	}

	// A schedule posts on its own interval, and can replace the old board.
	ch.reset()
	if r := setup(channelOpt + `,{"name":"mode","type":3,"value":"every"},{"name":"every_minutes","type":4,"value":5},{"name":"delete_old","type":5,"value":true}`); !strings.Contains(r.Content, "every 5 min, replacing the old one") {
		t.Fatalf("schedule setup: %q", r.Content)
	}
	ch.reset()
	refresh()
	if got := ch.actionList(); got != "" {
		t.Fatalf("a schedule shouldn't post again straight away, did %q", got)
	}
	s.db.Exec(`UPDATE boards SET posted_at = posted_at - 600`)
	refresh()
	if got := ch.actionList(); got != "post,delete" {
		t.Fatalf("a due schedule should repost and delete the old, did %q", got)
	}
	if !strings.Contains(ch.last(), "<t:") {
		t.Fatalf("a scheduled board should carry a timestamp: %q", ch.last())
	}

	// Listing and stopping.
	list := h.send(alice, `{"type":2,"token":"tok","application_id":"app","context":0,"guild_id":"g1",`+
		`"data":{"name":"board","options":[{"name":"list","type":1,"options":[]}]},`+
		`"member":{"permissions":"8","user":{"id":"`+alice.DiscordID+`","username":"alice"}}}`)
	if !strings.Contains(list.Content, "<#c1> — every 5 min") {
		t.Fatalf("list: %q", list.Content)
	}
	stop := h.send(alice, `{"type":2,"token":"tok","application_id":"app","context":0,"guild_id":"g1",`+
		`"data":{"name":"board","options":[{"name":"stop","type":1,"options":[`+channelOpt+`]}]},`+
		`"member":{"permissions":"32","user":{"id":"`+alice.DiscordID+`","username":"alice"}}}`)
	if !strings.Contains(stop.Content, "Stopped") {
		t.Fatalf("stop: %q", stop.Content)
	}
	if boards, _ := s.Boards(ctx, "g1"); len(boards) != 0 {
		t.Fatalf("board should be gone: %+v", boards)
	}

	// A channel the bot can't post in fails at setup, rather than silently.
	ch.fail = true
	if r := setup(channelOpt + `,{"name":"mode","type":3,"value":"live"}`); !strings.Contains(r.Content, "couldn't post") {
		t.Fatalf("unpostable channel: %q", r.Content)
	}
	if boards, _ := s.Boards(ctx, "g1"); len(boards) != 0 {
		t.Fatalf("a board that can't post shouldn't be kept: %+v", boards)
	}
}

func TestBoardTableAndStatus(t *testing.T) {
	// The table pads by visible width, ignoring the colour codes.
	orders := []Order{{
		ID: 3, From: "AIC", To: "CIS", Amount: 1_000_000, Remaining: 1_000_000,
		Owner:     Trader{Handle: "evil", CompanyUser: "Dr.Evil_", CompanyCode: "ORGS", CorpCode: "B"},
		CreatedAt: time.Now().Add(-11 * 24 * time.Hour),
	}, {
		ID: 14, From: "ICA", To: "CIS", Amount: 500_000, Remaining: 500_000,
		Owner:     Trader{Handle: "v", CompanyUser: "Vence62", CompanyCode: "LNS"},
		CreatedAt: time.Now().Add(-7 * 24 * time.Hour),
	}}
	msg := boardMessage(orders, OrderFilter{}, false, "")
	lines := strings.Split(strings.Trim(strings.TrimPrefix(msg, "```ansi\n"), "`\n"), "\n")
	width := visWidth(lines[0])
	for i, l := range lines {
		if visWidth(l) != width {
			t.Errorf("line %d is %d wide, want %d: %q", i, visWidth(l), width, l)
		}
	}
	for _, want := range []string{"[B] Dr.Evil_ (ORGS)", "Vence62 (LNS)", "11d", "7d",
		ansiOrderID + "#3" + ansiReset, currencyColor["AIC"] + "AIC" + ansiReset, ansiAmount + "500,000" + ansiReset} {
		if !strings.Contains(msg, want) {
			t.Errorf("board is missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "│   500,000 / 500,000 │") {
		t.Error("the amount column should be centred")
	}

	for _, c := range []struct {
		n    int64
		want string
	}{{0, "0 posted orders"}, {950, "950 posted orders"}, {12_500, "12.5k posted orders"},
		{2_500_000, "2.5m posted orders"}, {3_000_000, "3m posted orders"}, {1_250_000_000, "1.2b posted orders"}, {1_960_000_000, "2b posted orders"}} {
		if got := statusText(c.n); got != c.want {
			t.Errorf("statusText(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestOpenVolume(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	if v, _ := s.OpenVolume(ctx); v != 0 {
		t.Fatalf("empty board: %d", v)
	}
	mustPlace(t, s, alice, "AIC", "NCC", 100, false)
	mustPlace(t, s, bob, "CIS", "ICA", 50, false)
	s.Fill(ctx, 1, bob, 30)
	if v, _ := s.OpenVolume(ctx); v != 120 {
		t.Fatalf("after a fill: %d, want 120", v)
	}
	s.Cancel(ctx, 2, bob.ID)
	if v, _ := s.OpenVolume(ctx); v != 70 {
		t.Fatalf("after a cancel: %d, want 70", v)
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
