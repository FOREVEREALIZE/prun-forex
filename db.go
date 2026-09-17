package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var Currencies = []string{"AIC", "CIS", "ICA", "NCC"}

func validCurrency(c string) bool {
	for _, x := range Currencies {
		if x == c {
			return true
		}
	}
	return false
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         INTEGER PRIMARY KEY,
	discord_id TEXT NOT NULL UNIQUE,
	username   TEXT NOT NULL DEFAULT '',
	avatar     TEXT NOT NULL DEFAULT '',
	linked_at  INTEGER,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS orders (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id),
	from_cur   TEXT NOT NULL,
	to_cur     TEXT NOT NULL,
	amount     INTEGER NOT NULL CHECK (amount > 0),
	remaining  INTEGER NOT NULL CHECK (remaining >= 0 AND remaining <= amount),
	status     TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','filled','cancelled')),
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS orders_open ON orders(status, from_cur, to_cur, created_at);
CREATE INDEX IF NOT EXISTS orders_user ON orders(user_id, created_at);
CREATE TABLE IF NOT EXISTS fills (
	id         INTEGER PRIMARY KEY,
	order_id   INTEGER NOT NULL REFERENCES orders(id),
	filler_id  INTEGER NOT NULL REFERENCES users(id),
	amount     INTEGER NOT NULL CHECK (amount > 0),
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS fills_order ON fills(order_id);
CREATE INDEX IF NOT EXISTS fills_filler ON fills(filler_id);
CREATE TABLE IF NOT EXISTS notifications (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id),
	message    TEXT NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	next_at    INTEGER NOT NULL,
	sent_at    INTEGER,
	failed_at  INTEGER,
	last_error TEXT
);
CREATE INDEX IF NOT EXISTS notifications_pending ON notifications(next_at) WHERE sent_at IS NULL AND failed_at IS NULL;
`

// userError is a problem with the request that's safe to show the user.
type userError string

func (e userError) Error() string { return string(e) }

func isUserError(err error) bool {
	var ue userError
	return errors.As(err, &ue)
}

const (
	ErrNotFound = userError("Order not found.")
	ErrOwnOrder = userError("That's your own order.")
	ErrTooMuch  = userError("That order no longer has that much remaining.")
	ErrClosed   = userError("That order is no longer open.")
)

type Store struct {
	db      *sql.DB
	baseURL string
}

func OpenStore(path, baseURL string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	// Immediate transactions take the write lock up front, so concurrent
	// fills queue behind each other instead of failing on lock upgrade.
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db, baseURL: baseURL}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type User struct {
	ID        int64
	DiscordID string
	Username  string
	Avatar    string
	Linked    bool
}

func (u *User) AvatarURL() string {
	if u.Avatar == "" || strings.HasPrefix(u.DiscordID, "dev-") {
		return ""
	}
	return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=64", u.DiscordID, u.Avatar)
}

type Order struct {
	ID        int64
	UserID    int64
	Owner     string
	From      string
	To        string
	Amount    int64
	Remaining int64
	Status    string
	CreatedAt time.Time
	Fills     []Fill
}

func (o Order) Filled() int64 { return o.Amount - o.Remaining }
func (o Order) Pct() int64    { return o.Filled() * 100 / o.Amount }

type Fill struct {
	ID        int64
	OrderID   int64
	Filler    string
	Amount    int64
	CreatedAt time.Time
}

// Trade is a fill seen from one user's side: what they send and receive in-game.
type Trade struct {
	OrderID      int64
	Counterparty string
	Amount       int64
	Send         string
	Receive      string
	CreatedAt    time.Time
}

// UpsertUser creates or refreshes a user by Discord ID. With link=true it also
// marks them linked; otherwise the existing link state is kept.
func (s *Store) UpsertUser(ctx context.Context, discordID, username, avatar string, link bool) (*User, error) {
	now := time.Now().Unix()
	var linkedAt any
	if link {
		linkedAt = now
	}
	u := &User{DiscordID: discordID, Username: username, Avatar: avatar}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO users (discord_id, username, avatar, linked_at, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (discord_id) DO UPDATE SET
			username = excluded.username,
			avatar = excluded.avatar,
			linked_at = COALESCE(excluded.linked_at, users.linked_at)
		RETURNING id, linked_at IS NOT NULL`,
		discordID, username, avatar, linkedAt, now).Scan(&u.ID, &u.Linked)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) Unlink(ctx context.Context, discordID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET linked_at = NULL WHERE discord_id = ?`, discordID)
	return err
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	u := &User{ID: id}
	err := s.db.QueryRowContext(ctx,
		`SELECT discord_id, username, avatar, linked_at IS NOT NULL FROM users WHERE id = ?`, id).
		Scan(&u.DiscordID, &u.Username, &u.Avatar, &u.Linked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

type OrderFilter struct {
	Status        string // open, filled, cancelled, or "" for all
	From, To      string // optional
	ExcludeUserID int64
	UserID        int64
	Limit         int
}

const orderCols = `o.id, o.user_id, u.username, o.from_cur, o.to_cur, o.amount, o.remaining, o.status, o.created_at`

type scanner interface{ Scan(...any) error }

func scanOrder(r scanner) (Order, error) {
	var o Order
	var created int64
	err := r.Scan(&o.ID, &o.UserID, &o.Owner, &o.From, &o.To, &o.Amount, &o.Remaining, &o.Status, &created)
	o.CreatedAt = time.Unix(created, 0)
	return o, err
}

func (s *Store) ListOrders(ctx context.Context, f OrderFilter) ([]Order, error) {
	return listOrders(ctx, s.db, f)
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func listOrders(ctx context.Context, q querier, f OrderFilter) ([]Order, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "o.status = ?")
		args = append(args, f.Status)
	}
	if f.From != "" {
		where = append(where, "o.from_cur = ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		where = append(where, "o.to_cur = ?")
		args = append(args, f.To)
	}
	if f.ExcludeUserID != 0 {
		where = append(where, "o.user_id != ?")
		args = append(args, f.ExcludeUserID)
	}
	if f.UserID != 0 {
		where = append(where, "o.user_id = ?")
		args = append(args, f.UserID)
	}
	query := `SELECT ` + orderCols + ` FROM orders o JOIN users u ON u.id = o.user_id`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// Open orders are matched oldest first, so list them that way too.
	if f.Status == "open" {
		query += " ORDER BY o.created_at, o.id"
	} else {
		query += " ORDER BY (o.status = 'open') DESC, o.created_at DESC, o.id DESC"
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	query += fmt.Sprintf(" LIMIT %d", f.Limit)

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) OrderByID(ctx context.Context, id int64) (*Order, error) {
	o, err := scanOrder(s.db.QueryRowContext(ctx,
		`SELECT `+orderCols+` FROM orders o JOIN users u ON u.id = o.user_id WHERE o.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.order_id, u.username, f.amount, f.created_at
		FROM fills f JOIN users u ON u.id = f.filler_id
		WHERE f.order_id = ? ORDER BY f.created_at, f.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f Fill
		var created int64
		if err := rows.Scan(&f.ID, &f.OrderID, &f.Filler, &f.Amount, &created); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(created, 0)
		o.Fills = append(o.Fills, f)
	}
	return &o, rows.Err()
}

// Trades lists fills the user took part in, as owner or filler, newest first.
func (s *Store) Trades(ctx context.Context, userID int64, limit int) ([]Trade, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id, o.user_id, owner.username, filler.username, o.from_cur, o.to_cur, f.amount, f.created_at
		FROM fills f
		JOIN orders o ON o.id = f.order_id
		JOIN users owner ON owner.id = o.user_id
		JOIN users filler ON filler.id = f.filler_id
		WHERE o.user_id = ? OR f.filler_id = ?
		ORDER BY f.created_at DESC, f.id DESC
		LIMIT ?`, userID, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		var t Trade
		var ownerID, created int64
		var owner, filler, from, to string
		if err := rows.Scan(&t.OrderID, &ownerID, &owner, &filler, &from, &to, &t.Amount, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0)
		if ownerID == userID {
			// The owner offered `from` and wanted `to`.
			t.Counterparty, t.Send, t.Receive = filler, from, to
		} else {
			t.Counterparty, t.Send, t.Receive = owner, to, from
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Cancel(ctx context.Context, orderID, userID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE orders SET status = 'cancelled' WHERE id = ? AND user_id = ? AND status = 'open'`, orderID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrClosed
	}
	return nil
}

type Notification struct {
	ID        int64
	UserID    int64
	DiscordID string
	Message   string
	Attempts  int
}

func (s *Store) PendingNotifications(ctx context.Context, limit int) ([]Notification, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.user_id, u.discord_id, n.message, n.attempts
		FROM notifications n JOIN users u ON u.id = n.user_id
		WHERE n.sent_at IS NULL AND n.failed_at IS NULL AND n.next_at <= ?
		ORDER BY n.next_at, n.id LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.UserID, &n.DiscordID, &n.Message, &n.Attempts); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) MarkSent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET sent_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

func (s *Store) MarkRetry(ctx context.Context, id int64, attempts int, delay time.Duration, cause error) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET attempts = ?, next_at = ?, last_error = ? WHERE id = ?`,
		attempts, time.Now().Add(delay).Unix(), cause.Error(), id)
	return err
}

// MarkFailed gives up on a notification. With unlink, the user is also
// unlinked, since Discord told us the bot can't reach them.
func (s *Store) MarkFailed(ctx context.Context, n Notification, cause error, unlink bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE notifications SET failed_at = ?, attempts = attempts + 1, last_error = ? WHERE id = ?`,
		time.Now().Unix(), cause.Error(), n.ID); err != nil {
		return err
	}
	if unlink {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET linked_at = NULL WHERE id = ?`, n.UserID); err != nil {
			return err
		}
		// No point retrying anything else queued for them.
		if _, err := tx.ExecContext(ctx, `UPDATE notifications SET failed_at = ?, last_error = 'user unreachable' WHERE user_id = ? AND sent_at IS NULL AND failed_at IS NULL`,
			time.Now().Unix(), n.UserID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
