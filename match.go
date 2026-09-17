package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Match describes open orders that could fill a new order right away.
type Match struct {
	From, To  string
	Amount    int64
	Legs      []MatchLeg // oldest first
	Available int64      // how much of Amount the legs cover
}

type MatchLeg struct {
	Order
	Take int64
}

func (m Match) Full() bool      { return m.Available >= m.Amount }
func (m Match) Leftover() int64 { return m.Amount - m.Available }

// FindMatch looks for other users' open orders going the opposite way.
func (s *Store) FindMatch(ctx context.Context, userID int64, from, to string, amount int64) (Match, error) {
	m := Match{From: from, To: to, Amount: amount}
	counters, err := s.ListOrders(ctx, OrderFilter{Status: "open", From: to, To: from, ExcludeUserID: userID})
	if err != nil {
		return m, err
	}
	for _, c := range counters {
		if m.Available == amount {
			break
		}
		n := min(amount-m.Available, c.Remaining)
		m.Legs = append(m.Legs, MatchLeg{Order: c, Take: n})
		m.Available += n
	}
	return m, nil
}

// ExistingOrder returns the user's most recent open order for the same pair, or nil.
func (s *Store) ExistingOrder(ctx context.Context, userID int64, from, to string) (*Order, error) {
	o, err := scanOrder(s.db.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders o JOIN users u ON u.id = o.user_id
		WHERE o.user_id = ? AND o.status = 'open' AND o.from_cur = ? AND o.to_cur = ?
		ORDER BY o.created_at DESC, o.id DESC LIMIT 1`, userID, from, to))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

type PlaceResult struct {
	Filled    int64  // taken from existing orders
	Fills     int    // how many orders were filled from
	Order     *Order // the order holding the leftover, if any
	Increased bool   // Order is an existing order that was topped up
}

// PlaceOrder creates an order. With take=true it first fills matching
// counter-orders (oldest first), then posts whatever is left: as a new order,
// or added to the user's open order intoID if that's non-zero. The matching is
// redone inside the transaction, so a stale preview can't overfill anything.
func (s *Store) PlaceOrder(ctx context.Context, user *User, from, to string, amount int64, take bool, intoID int64) (PlaceResult, error) {
	var res PlaceResult
	if err := validateOrder(from, to, amount); err != nil {
		return res, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	left := amount
	if take {
		counters, err := listOrders(ctx, tx, OrderFilter{Status: "open", From: to, To: from, ExcludeUserID: user.ID})
		if err != nil {
			return res, err
		}
		for _, c := range counters {
			if left == 0 {
				break
			}
			n := min(left, c.Remaining)
			if err := s.applyFill(ctx, tx, c, user, n); err != nil {
				return res, err
			}
			left -= n
			res.Filled += n
			res.Fills++
		}
	}
	if left > 0 && intoID != 0 {
		o, err := scanOrder(tx.QueryRowContext(ctx, `
			UPDATE orders SET amount = amount + ?1, remaining = remaining + ?1
			WHERE id = ?2 AND user_id = ?3 AND status = 'open' AND from_cur = ?4 AND to_cur = ?5
			RETURNING id, user_id, ?6, from_cur, to_cur, amount, remaining, status, created_at`,
			left, intoID, user.ID, from, to, user.Username))
		if errors.Is(err, sql.ErrNoRows) {
			return res, userError(fmt.Sprintf("Order #%d is no longer open, so nothing was added to it.", intoID))
		}
		if err != nil {
			return res, err
		}
		res.Order, res.Increased = &o, true
	} else if left > 0 {
		now := time.Now()
		var id int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO orders (user_id, from_cur, to_cur, amount, remaining, created_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
			user.ID, from, to, left, left, now.Unix()).Scan(&id)
		if err != nil {
			return res, err
		}
		res.Order = &Order{ID: id, UserID: user.ID, Owner: user.Username, From: from, To: to,
			Amount: left, Remaining: left, Status: "open", CreatedAt: now}
	}
	return res, tx.Commit()
}

// Fill takes amount (0 = everything remaining) from someone else's order.
func (s *Store) Fill(ctx context.Context, orderID int64, filler *User, amount int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	o, err := scanOrder(tx.QueryRowContext(ctx,
		`SELECT `+orderCols+` FROM orders o JOIN users u ON u.id = o.user_id WHERE o.id = ?`, orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	switch {
	case o.UserID == filler.ID:
		return 0, ErrOwnOrder
	case o.Status != "open":
		return 0, ErrClosed
	case amount < 0:
		return 0, userError("Amount must be positive.")
	case amount == 0:
		amount = o.Remaining
	case amount > o.Remaining:
		return 0, ErrTooMuch
	}
	if err := s.applyFill(ctx, tx, o, filler, amount); err != nil {
		return 0, err
	}
	return amount, tx.Commit()
}

func (s *Store) applyFill(ctx context.Context, tx *sql.Tx, o Order, filler *User, n int64) error {
	// The guarded UPDATE is the real overfill check; the caller's read may be stale.
	res, err := tx.ExecContext(ctx, `
		UPDATE orders SET remaining = remaining - ?1,
			status = CASE WHEN remaining - ?1 = 0 THEN 'filled' ELSE status END
		WHERE id = ?2 AND status = 'open' AND remaining >= ?1 AND user_id != ?3`,
		n, o.ID, filler.ID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrTooMuch
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO fills (order_id, filler_id, amount, created_at) VALUES (?, ?, ?, ?)`,
		o.ID, filler.ID, n, now); err != nil {
		return err
	}
	return queueDM(ctx, tx, o.UserID, s.fillMessage(o, filler, n))
}

// queueDM adds a DM to the outbox, if the user is still linked.
func queueDM(ctx context.Context, tx *sql.Tx, userID int64, msg string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO notifications (user_id, message, next_at)
		SELECT id, ?, ? FROM users WHERE id = ? AND linked_at IS NOT NULL`,
		msg, time.Now().Unix(), userID)
	return err
}

func (s *Store) fillMessage(o Order, filler *User, n int64) string {
	left := o.Remaining - n
	head := fmt.Sprintf("**%s** filled **%d** of your **%s → %s** order `#%d`", filler.Username, n, o.From, o.To, o.ID)
	if left == 0 {
		head += " and it's now **fully filled** ✅"
	} else {
		head += fmt.Sprintf(": **%d / %d** still open", left, o.Amount)
	}
	msg := fmt.Sprintf("%s\nYou send them **%d %s**; they send you **%d %s**. Decide on the site who sends the CONT.", head, n, o.From, n, o.To)
	if filler.DiscordID != "" && !isDevID(filler.DiscordID) {
		msg += fmt.Sprintf("\nContact: <@%s>", filler.DiscordID)
	}
	if s.baseURL != "" {
		msg += "\n" + s.baseURL
	}
	return msg
}

func validateOrder(from, to string, amount int64) error {
	switch {
	case !validCurrency(from) || !validCurrency(to):
		return userError("Unknown currency.")
	case from == to:
		return userError("Pick two different currencies.")
	case amount <= 0:
		return userError("Amount must be a positive whole number.")
	case amount > 1_000_000_000_000:
		return userError("Amount is too large.")
	}
	return nil
}
