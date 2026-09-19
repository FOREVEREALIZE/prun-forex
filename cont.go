package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Settling a trade in-game takes a CONT (contract) that one side sends and the
// other accepts. For each fill the two sides first agree who sends it: either
// volunteers, or asks the other, who can accept or ask back. The sender marks
// it sent, then each side marks the trade fulfilled, which hides it for them.

// ContState is where a trade's CONT stands, from the viewer's side.
type ContState string

const (
	ContUndecided  ContState = "undecided"  // nobody has volunteered or asked
	ContAskedThem  ContState = "asked-them" // I asked them to send it
	ContAskedMe    ContState = "asked-me"   // they asked me to send it
	ContMine       ContState = "mine"       // I send it, not sent yet
	ContTheirs     ContState = "theirs"     // they send it, not sent yet
	ContSentByMe   ContState = "sent-by-me"
	ContSentByThem ContState = "sent-by-them"
	ContSent       ContState = "sent" // sent, sender unknown (settled before CONTs existed)
)

// ContAction is something a trader can do to move the CONT along.
type ContAction string

const (
	ActSendMyself ContAction = "self"    // volunteer, or accept their request
	ActRequest    ContAction = "request" // ask them, or ask back
	ActMarkSent   ContAction = "sent"    // the sender has sent the CONT
	ActFulfill    ContAction = "fulfill" // my side is done
	ActUnfulfill  ContAction = "unfulfill"
)

var (
	errTradeNotFound = userError("Trade not found.")
	errTradeChanged  = userError("That trade changed in the meantime, take another look.")
)

// Trade is a fill seen from one user's side: what they send and receive in-game.
type Trade struct {
	FillID        int64
	OrderID       int64
	Counterparty  Trader
	Amount        int64
	Send          string
	Receive       string
	CreatedAt     time.Time
	Cont          ContState
	Fulfilled     bool // by me
	TheyFulfilled bool
}

func contState(me, other int64, contractor, requestFrom sql.NullInt64, sent bool) ContState {
	switch {
	case sent && contractor.Valid && contractor.Int64 == me:
		return ContSentByMe
	case sent && contractor.Valid:
		return ContSentByThem
	case sent:
		return ContSent
	case contractor.Valid && contractor.Int64 == me:
		return ContMine
	case contractor.Valid:
		return ContTheirs
	case requestFrom.Valid && requestFrom.Int64 == me:
		return ContAskedThem
	case requestFrom.Valid && requestFrom.Int64 == other:
		return ContAskedMe
	}
	return ContUndecided
}

// Trades lists fills the user took part in, as owner or filler, newest first.
// Ones the user has marked fulfilled are left out unless includeFulfilled.
func (s *Store) Trades(ctx context.Context, userID int64, includeFulfilled bool, limit int) ([]Trade, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, o.id, o.user_id, f.filler_id, `+traderCols("owner")+`, `+traderCols("filler")+`, o.from_cur, o.to_cur,
			f.amount, f.created_at, f.contractor_id, f.cont_request_from, f.cont_sent_at IS NOT NULL,
			f.owner_fulfilled_at IS NOT NULL, f.filler_fulfilled_at IS NOT NULL,
			CASE WHEN o.user_id = ?1 THEN f.owner_fulfilled_at ELSE f.filler_fulfilled_at END IS NOT NULL AS mine_fulfilled
		FROM fills f
		JOIN orders o ON o.id = f.order_id
		JOIN users owner ON owner.id = o.user_id
		JOIN users filler ON filler.id = f.filler_id
		WHERE (o.user_id = ?1 OR f.filler_id = ?1) AND (?2 OR mine_fulfilled = 0)
		ORDER BY mine_fulfilled, f.created_at DESC, f.id DESC
		LIMIT ?3`, userID, includeFulfilled, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		var t Trade
		var ownerID, fillerID, created int64
		var owner, filler Trader
		var from, to string
		var contractor, requestFrom sql.NullInt64
		var sent, ownerFulfilled, fillerFulfilled, mineFulfilled bool
		dest := []any{&t.FillID, &t.OrderID, &ownerID, &fillerID}
		dest = append(dest, owner.dest()...)
		dest = append(dest, filler.dest()...)
		dest = append(dest, &from, &to, &t.Amount, &created, &contractor, &requestFrom, &sent,
			&ownerFulfilled, &fillerFulfilled, &mineFulfilled)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0)
		otherID := ownerID
		if ownerID == userID {
			// The owner offered `from` and wanted `to`.
			otherID = fillerID
			t.Counterparty, t.Send, t.Receive = filler, from, to
			t.Fulfilled, t.TheyFulfilled = ownerFulfilled, fillerFulfilled
		} else {
			t.Counterparty, t.Send, t.Receive = owner, to, from
			t.Fulfilled, t.TheyFulfilled = fillerFulfilled, ownerFulfilled
		}
		t.Cont = contState(userID, otherID, contractor, requestFrom, sent)
		out = append(out, t)
	}
	return out, rows.Err()
}

// FulfilledCount is how many of the user's trades they've marked fulfilled.
func (s *Store) FulfilledCount(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM fills f JOIN orders o ON o.id = f.order_id
		WHERE (o.user_id = ?1 AND f.owner_fulfilled_at IS NOT NULL)
		   OR (f.filler_id = ?1 AND f.filler_fulfilled_at IS NOT NULL)`, userID).Scan(&n)
	return n, err
}

// ContAct applies a CONT action to a fill on behalf of user, DMing the other
// side where it concerns them. It returns the other side.
func (s *Store) ContAct(ctx context.Context, fillID int64, user *User, action ContAction) (Trader, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Trader{}, err
	}
	defer tx.Rollback()

	var orderID, ownerID, fillerID, amount int64
	var from, to string
	var ownerT, fillerT Trader
	var contractor, requestFrom, sentAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT o.id, o.user_id, f.filler_id, f.amount, o.from_cur, o.to_cur, `+traderCols("owner")+`, `+traderCols("filler")+`,
			f.contractor_id, f.cont_request_from, f.cont_sent_at
		FROM fills f
		JOIN orders o ON o.id = f.order_id
		JOIN users owner ON owner.id = o.user_id
		JOIN users filler ON filler.id = f.filler_id
		WHERE f.id = ?`, fillID).
		Scan(append(append(append([]any{&orderID, &ownerID, &fillerID, &amount, &from, &to}, ownerT.dest()...), fillerT.dest()...),
			&contractor, &requestFrom, &sentAt)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Trader{}, errTradeNotFound
	}
	if err != nil {
		return Trader{}, err
	}

	me := user.ID
	isOwner := me == ownerID
	if !isOwner && me != fillerID {
		return Trader{}, errTradeNotFound
	}
	otherID, other := fillerID, fillerT
	// What the other side sends and receives: the owner offered `from`.
	theySend, theyGet := to, from
	fulfilledCol := "owner_fulfilled_at"
	if !isOwner {
		otherID, other = ownerID, ownerT
		theySend, theyGet = from, to
		fulfilledCol = "filler_fulfilled_at"
	}
	trade := fmt.Sprintf("your trade on order `#%d` (you provide **%s %s**, they provide **%s %s**)",
		orderID, formatInt(amount), theySend, formatInt(amount), theyGet)
	link := ""
	if s.baseURL != "" {
		link = "\n" + s.baseURL
	}
	requestedByOther := requestFrom.Valid && requestFrom.Int64 == otherID

	var dm string
	switch action {
	case ActSendMyself:
		if contractor.Valid || sentAt.Valid {
			return Trader{}, errTradeChanged
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fills SET contractor_id = ?, cont_request_from = NULL WHERE id = ?`, me, fillID); err != nil {
			return Trader{}, err
		}
		if requestedByOther {
			dm = fmt.Sprintf("**%s** accepted: they'll send the CONT for %s. I'll DM you once it's sent.", user.Name(), trade)
		} else {
			dm = fmt.Sprintf("**%s** will send the CONT for %s. I'll DM you once it's sent.", user.Name(), trade)
		}

	case ActRequest:
		if contractor.Valid || sentAt.Valid || (requestFrom.Valid && requestFrom.Int64 == me) {
			return Trader{}, errTradeChanged
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fills SET cont_request_from = ? WHERE id = ?`, me, fillID); err != nil {
			return Trader{}, err
		}
		if requestedByOther {
			dm = fmt.Sprintf("**%s** asked you to send the CONT instead, for %s. Accept or ask them again:", user.Name(), trade)
		} else {
			dm = fmt.Sprintf("**%s** asks you to send the CONT for %s. Accept or ask them instead:", user.Name(), trade)
		}

	case ActMarkSent:
		if !contractor.Valid || contractor.Int64 != me || sentAt.Valid {
			return Trader{}, errTradeChanged
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fills SET cont_sent_at = ? WHERE id = ?`, time.Now().Unix(), fillID); err != nil {
			return Trader{}, err
		}
		dm = fmt.Sprintf("**%s** sent the CONT for %s. Accept it in-game, then mark the trade fulfilled.", user.Name(), trade)

	case ActFulfill, ActUnfulfill:
		if !sentAt.Valid {
			return Trader{}, userError("The CONT hasn't been sent yet.")
		}
		var at any
		if action == ActFulfill {
			at = time.Now().Unix()
		}
		// fulfilledCol is one of two constants above, never user input.
		if _, err := tx.ExecContext(ctx, `UPDATE fills SET `+fulfilledCol+` = ? WHERE id = ?`, at, fillID); err != nil {
			return Trader{}, err
		}

	default:
		return Trader{}, userError("Unknown action.")
	}

	if dm != "" {
		if err := queueDM(ctx, tx, otherID, dm+link); err != nil {
			return Trader{}, err
		}
	}
	return other, tx.Commit()
}
