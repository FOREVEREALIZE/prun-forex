package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// The order board rendered for Discord: a box-drawn table in an "ansi" code
// block, which Discord colours in. Phones wrap it badly, so there's a plain
// list too (/orders mobile:True).

const (
	ansiReset   = "\u001b[0;0m"
	ansiOrderID = "\u001b[0;33m" // yellow
	ansiAmount  = "\u001b[0;32m" // green
)

// Each currency keeps the same colour wherever it shows up.
var currencyColor = map[string]string{
	"AIC": "\u001b[0;34m", // blue
	"CIS": "\u001b[0;31m", // red
	"ICA": "\u001b[0;35m", // magenta
	"NCC": "\u001b[0;36m", // cyan
}

func paint(color, text string) string {
	if color == "" {
		return text
	}
	return color + text + ansiReset
}

var ansiCodes = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// visWidth is how wide a cell looks once Discord has eaten the colour codes.
func visWidth(s string) int { return len([]rune(ansiCodes.ReplaceAllString(s, ""))) }

type align int

const (
	alignLeft align = iota
	alignCenter
)

type column struct {
	header string
	align  align
}

// boardTable draws the rows as a box-drawn table, padding by visible width.
func boardTable(cols []column, rows [][]string) string {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len([]rune(c.header))
	}
	for _, r := range rows {
		for i, cell := range r {
			if w := visWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}
	pad := func(cell string, i int, center bool) string {
		space := widths[i] - visWidth(cell)
		if center {
			left := space / 2
			return strings.Repeat(" ", left) + cell + strings.Repeat(" ", space-left)
		}
		return cell + strings.Repeat(" ", space)
	}
	rule := func(left, mid, right, fill string) string {
		parts := make([]string, len(cols))
		for i := range cols {
			parts[i] = strings.Repeat(fill, widths[i]+2)
		}
		return left + strings.Join(parts, mid) + right
	}
	line := func(cells []string, header bool) string {
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = " " + pad(c, i, !header && cols[i].align == alignCenter) + " "
		}
		return "│" + strings.Join(parts, "│") + "│"
	}

	var sb strings.Builder
	sb.WriteString(rule("┌", "┬", "┐", "─") + "\n")
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.header
	}
	sb.WriteString(line(headers, true) + "\n")
	sb.WriteString(rule("╞", "╪", "╡", "═") + "\n")
	for _, r := range rows {
		sb.WriteString(line(r, false) + "\n")
	}
	sb.WriteString(rule("└", "┴", "┘", "─"))
	return sb.String()
}

// tableName is the trader as the board shows them: "[CORP] User (CODE)".
func tableName(t Trader) string {
	if t.CompanyCode == "" {
		return t.Handle
	}
	name := t.CompanyUser + " (" + t.CompanyCode + ")"
	if t.CorpCode != "" {
		name = "[" + t.CorpCode + "] " + name
	}
	return name
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// discordMessageLimit leaves room under Discord's 2000 characters for the
// footer the board adds afterwards.
const discordMessageLimit = 1800

// boardMessage is the whole message for a board: a heading and the table (or a
// plain list). Rows are dropped from the end until it fits in a Discord
// message, since the colour codes count towards the limit.
func boardMessage(orders []Order, filter OrderFilter, mobile bool, heading string) string {
	for rows := len(orders); ; rows-- {
		msg := renderBoard(orders[:rows], len(orders)-rows, filter, mobile, heading)
		if rows == 0 || len(msg) <= discordMessageLimit {
			return msg
		}
	}
}

func renderBoard(orders []Order, more int, filter OrderFilter, mobile bool, heading string) string {
	pair := ""
	if filter.From != "" || filter.To != "" {
		pair = fmt.Sprintf(" %s → %s", orDash(filter.From), orDash(filter.To))
	}
	var sb strings.Builder
	if heading != "" {
		sb.WriteString(heading + "\n")
	}
	if len(orders) == 0 {
		sb.WriteString(fmt.Sprintf("No open orders%s right now.", pair))
		return sb.String()
	}
	if mobile {
		for _, o := range orders {
			fmt.Fprintf(&sb, "`#%d` **%s → %s** — **%s** open of %s — %s\n",
				o.ID, o.From, o.To, formatInt(o.Remaining), formatInt(o.Amount), o.Owner.Name())
		}
	} else {
		cols := []column{
			{header: "Ord#"}, {header: "Trader"}, {header: "Frm"}, {header: "To"},
			{header: "Amount Open", align: alignCenter}, {header: "Age"},
		}
		rows := make([][]string, 0, len(orders))
		for _, o := range orders {
			rows = append(rows, []string{
				paint(ansiOrderID, fmt.Sprintf("#%d", o.ID)),
				truncate(tableName(o.Owner), 28),
				paint(currencyColor[o.From], o.From),
				paint(currencyColor[o.To], o.To),
				paint(ansiAmount, formatInt(o.Remaining)) + " / " + formatInt(o.Amount),
				ago(o.CreatedAt),
			})
		}
		sb.WriteString("```ansi\n" + boardTable(cols, rows) + "\n```")
	}
	if more > 0 {
		fmt.Fprintf(&sb, "\n…and %d more.", more)
	}
	return sb.String()
}

// Board is a channel where the bot keeps an order board posted.
type Board struct {
	ID        int64
	GuildID   string
	ChannelID string
	Mode      string // "live" (on changes) or "every" (on a schedule)
	Interval  int    // minutes, for "every"
	DeleteOld bool   // for "every": remove the previous board
	Mobile    bool
	From, To  string
	MessageID string
	Signature string // the order data last posted
	PostedAt  time.Time
}

func (b Board) Filter() OrderFilter {
	return OrderFilter{Status: "open", From: b.From, To: b.To, Limit: 25}
}

// Describe is how the board reads in /board list and after setup.
func (b Board) Describe() string {
	what := "when orders change"
	if b.Mode == "every" {
		what = fmt.Sprintf("every %d min", b.Interval)
		if b.DeleteOld {
			what += ", replacing the old one"
		}
	}
	extra := ""
	if b.From != "" || b.To != "" {
		extra += fmt.Sprintf(", %s → %s only", orDash(b.From), orDash(b.To))
	}
	if b.Mobile {
		extra += ", plain list"
	}
	return fmt.Sprintf("<#%s> — %s%s", b.ChannelID, what, extra)
}

// signature covers what the board shows apart from the ages, so a board is
// only touched when the orders themselves change.
func boardSignature(orders []Order) string {
	var sb strings.Builder
	for _, o := range orders {
		fmt.Fprintf(&sb, "%d:%d:%d;", o.ID, o.Remaining, o.Amount)
	}
	return sb.String()
}

func (s *Store) SaveBoard(ctx context.Context, b Board) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO boards (guild_id, channel_id, mode, interval_minutes, delete_old, mobile, from_cur, to_cur, created_at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
		ON CONFLICT (channel_id) DO UPDATE SET
			guild_id = excluded.guild_id, mode = excluded.mode, interval_minutes = excluded.interval_minutes,
			delete_old = excluded.delete_old, mobile = excluded.mobile,
			from_cur = excluded.from_cur, to_cur = excluded.to_cur,
			signature = '', failures = 0
		RETURNING id`,
		b.GuildID, b.ChannelID, b.Mode, b.Interval, b.DeleteOld, b.Mobile, b.From, b.To, time.Now().Unix()).Scan(&id)
	return id, err
}

const boardCols = `id, guild_id, channel_id, mode, interval_minutes, delete_old, mobile, from_cur, to_cur, message_id, signature, posted_at`

func scanBoard(r scanner) (Board, error) {
	var b Board
	var posted int64
	err := r.Scan(&b.ID, &b.GuildID, &b.ChannelID, &b.Mode, &b.Interval, &b.DeleteOld, &b.Mobile,
		&b.From, &b.To, &b.MessageID, &b.Signature, &posted)
	b.PostedAt = time.Unix(posted, 0)
	return b, err
}

// Boards lists the boards still being kept up to date, or those of one guild.
func (s *Store) Boards(ctx context.Context, guildID string) ([]Board, error) {
	q := `SELECT ` + boardCols + ` FROM boards WHERE failures < 5`
	var args []any
	if guildID != "" {
		q += ` AND guild_id = ?`
		args = append(args, guildID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) MarkBoardPosted(ctx context.Context, id int64, messageID, signature string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE boards SET message_id = ?, signature = ?, posted_at = ?, failures = 0, last_error = '' WHERE id = ?`,
		messageID, signature, time.Now().Unix(), id)
	return err
}

// BoardFailed counts a failure; after five in a row the board is left alone.
func (s *Store) BoardFailed(ctx context.Context, id int64, cause error) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE boards SET failures = failures + 1, last_error = ? WHERE id = ?`, cause.Error(), id)
	return err
}

// DeleteBoard stops a channel's board, returning whether there was one.
func (s *Store) DeleteBoard(ctx context.Context, guildID, channelID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM boards WHERE guild_id = ? AND channel_id = ?`, guildID, channelID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RunBoards keeps every registered board up to date until ctx is done.
func (b *Bot) RunBoards(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		boards, err := b.store.Boards(ctx, "")
		if err != nil {
			log.Printf("boards: %v", err)
			continue
		}
		for _, board := range boards {
			if err := b.refreshBoard(ctx, board); err != nil {
				log.Printf("board in %s: %v", board.ChannelID, err)
				b.store.BoardFailed(ctx, board.ID, err)
			}
		}
	}
}

// refreshBoard posts, edits or leaves a board alone, depending on its mode and
// whether the orders have changed since it was last posted.
func (b *Bot) refreshBoard(ctx context.Context, board Board) error {
	orders, err := b.store.ListOrders(ctx, board.Filter())
	if err != nil {
		return err
	}
	signature := boardSignature(orders)

	repost := board.MessageID == ""
	switch board.Mode {
	case "every":
		if !repost && time.Since(board.PostedAt) < time.Duration(board.Interval)*time.Minute {
			return nil
		}
		repost = true
	default: // live
		if !repost {
			if signature == board.Signature {
				return nil
			}
			// A new order bumps the board to the bottom of the channel;
			// fills and removals are just edited in place.
			if !onlyShrunk(board.Signature, signature) {
				repost = true
			}
		}
	}

	content := b.boardContent(board, orders)
	if !repost {
		if err := b.editMessage(ctx, board.ChannelID, board.MessageID, content); err != nil {
			if de, ok := err.(*discordError); ok && de.Status == http.StatusNotFound {
				repost = true // someone deleted it
			} else {
				return err
			}
		}
	}
	if !repost {
		return b.store.MarkBoardPosted(ctx, board.ID, board.MessageID, signature)
	}

	id, err := b.postMessage(ctx, board.ChannelID, content)
	if err != nil {
		return err
	}
	// The old board goes once the new one is up, so the channel keeps just one.
	if board.MessageID != "" && (board.Mode == "live" || board.DeleteOld) {
		if err := b.deleteMessage(ctx, board.ChannelID, board.MessageID); err != nil {
			log.Printf("board in %s: removing the old message: %v", board.ChannelID, err)
		}
	}
	return b.store.MarkBoardPosted(ctx, board.ID, id, signature)
}

func (b *Bot) boardContent(board Board, orders []Order) string {
	heading := "## PrUn Forex — open orders"
	if board.Mode == "every" {
		heading += fmt.Sprintf(" · <t:%d:R>", time.Now().Unix())
	}
	msg := boardMessage(orders, board.Filter(), board.Mobile, heading)
	if b.store.baseURL != "" {
		msg += fmt.Sprintf("\nTrade at %s, or with `/post` and `/fill`.", b.store.baseURL)
	}
	return msg
}

// onlyShrunk reports whether the new signature has no order the old one
// lacked, so nothing was added and the board can be edited where it is.
func onlyShrunk(oldSig, newSig string) bool {
	had := map[string]bool{}
	for _, entry := range strings.Split(oldSig, ";") {
		if id, _, ok := strings.Cut(entry, ":"); ok {
			had[id] = true
		}
	}
	for _, entry := range strings.Split(newSig, ";") {
		if id, _, ok := strings.Cut(entry, ":"); ok && !had[id] {
			return false
		}
	}
	return true
}

// cmdBoard handles /board setup, /board stop and /board list.
func (b *Bot) cmdBoard(ctx context.Context, in interaction) botMessage {
	if in.GuildID == "" {
		return botMessage{Content: "Order boards are set up in a server, not in a DM."}
	}
	if !in.canManageGuild() {
		return botMessage{Content: "You need the Manage Server permission to set up an order board."}
	}
	sub, _ := in.sub()
	switch sub {
	case "setup":
		return b.boardSetup(ctx, in)
	case "stop":
		channel := in.option("channel")
		stopped, err := b.store.DeleteBoard(ctx, in.GuildID, channel)
		switch {
		case err != nil:
			return oops(err)
		case !stopped:
			return botMessage{Content: fmt.Sprintf("There's no order board in <#%s>.", channel)}
		}
		return botMessage{Content: fmt.Sprintf("Stopped the order board in <#%s>. The last message stays where it is.", channel)}
	case "list":
		boards, err := b.store.Boards(ctx, in.GuildID)
		if err != nil {
			return oops(err)
		}
		if len(boards) == 0 {
			return botMessage{Content: "No order boards in this server yet. Set one up with `/board setup`."}
		}
		var sb strings.Builder
		sb.WriteString("**Order boards here:**\n")
		for _, board := range boards {
			sb.WriteString("· " + board.Describe() + "\n")
		}
		return botMessage{Content: sb.String()}
	}
	return botMessage{Content: "Unknown board command."}
}

func (b *Bot) boardSetup(ctx context.Context, in interaction) botMessage {
	board := Board{
		GuildID:   in.GuildID,
		ChannelID: in.option("channel"),
		Mode:      in.option("mode"),
		Interval:  int(in.optionInt("every_minutes")),
		DeleteOld: in.optionBool("delete_old"),
		Mobile:    in.optionBool("mobile"),
		From:      NormalizeCurrency(in.option("from")),
		To:        NormalizeCurrency(in.option("to")),
	}
	if board.ChannelID == "" {
		return botMessage{Content: "Pick a channel for the board."}
	}
	if board.Mode != "every" {
		board.Mode = "live"
		board.Interval, board.DeleteOld = 0, false
	} else if board.Interval < 5 {
		board.Interval = 60
	}

	id, err := b.store.SaveBoard(ctx, board)
	if err != nil {
		return oops(err)
	}
	board.ID = id

	// Post it right away, so a channel the bot can't write to fails here
	// rather than quietly in the background.
	if err := b.refreshBoard(ctx, board); err != nil {
		b.store.DeleteBoard(ctx, board.GuildID, board.ChannelID)
		log.Printf("board setup in %s: %v", board.ChannelID, err)
		return botMessage{Content: fmt.Sprintf("I couldn't post in <#%s>. Check that I'm in this server and allowed to send messages there.", board.ChannelID)}
	}
	return botMessage{Content: "Order board set up: " + board.Describe()}
}
