package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Everything the site can do is also a slash command in the bot's DM, and the
// DMs the bot sends carry buttons for the obvious next step. Pressing a button
// turns that DM into an up-to-date card for the trade.

// Discord button styles.
const (
	btnPrimary   = 1
	btnSecondary = 2
	btnSuccess   = 3
	btnDanger    = 4
)

func button(style int, label, customID string) map[string]any {
	return map[string]any{"type": 2, "style": style, "label": label, "custom_id": customID}
}

// row wraps buttons in an action row, dropping empty ones. Discord allows five
// buttons per row.
func row(buttons ...map[string]any) []any {
	var out []any
	for _, b := range buttons {
		if b != nil {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return []any{map[string]any{"type": 1, "components": out}}
}

// contButtons are the actions open to the viewer of a trade in this state.
func contButtons(state ContState, fillID int64) []any {
	id := func(a ContAction) string { return fmt.Sprintf("cont:%s:%d", a, fillID) }
	callOff := button(btnDanger, "Call off", id(ActCancel))
	switch state {
	case ContUndecided:
		return row(button(btnSuccess, "I'll send it", id(ActSendMyself)),
			button(btnSecondary, "Ask them", id(ActRequest)), callOff)
	case ContAskedThem:
		return row(button(btnSecondary, "I'll send it instead", id(ActSendMyself)), callOff)
	case ContAskedMe:
		return row(button(btnSuccess, "Accept", id(ActSendMyself)),
			button(btnSecondary, "Ask them instead", id(ActRequest)), callOff)
	case ContMine:
		return row(button(btnSuccess, "CONT sent", id(ActMarkSent)), callOff)
	case ContTheirs:
		return row(callOff)
	case ContSentByMe, ContSentByThem, ContSent:
		return row(button(btnSuccess, "Mark fulfilled", id(ActFulfill)))
	}
	return nil
}

// tradeLine is one trade as a single line, from the viewer's side.
func tradeLine(t Trade) string {
	return fmt.Sprintf("`#%d` order `#%d` with **%s** — you provide **%s %s**, they provide **%s %s** — %s",
		t.FillID, t.OrderID, t.Counterparty.Name(),
		formatInt(t.Amount), t.Send, formatInt(t.Amount), t.Receive, tradeState(t))
}

// tradeState says where the trade stands, from the viewer's side.
func tradeState(t Trade) string {
	them := t.Counterparty.Short()
	switch {
	case t.Fulfilled && t.TheyFulfilled:
		return "**fulfilled by both sides**"
	case t.Fulfilled:
		return fmt.Sprintf("**fulfilled by you**, waiting for %s", them)
	}
	switch t.Cont {
	case ContUndecided:
		return "**who sends the CONT?**"
	case ContAskedThem:
		return fmt.Sprintf("you asked %s to send the CONT", them)
	case ContAskedMe:
		return fmt.Sprintf("**%s asks you to send the CONT**", them)
	case ContMine:
		return "**you send the CONT**"
	case ContTheirs:
		return fmt.Sprintf("%s sends the CONT", them)
	case ContSentByMe:
		return "you sent the CONT"
	case ContSentByThem:
		return fmt.Sprintf("**%s sent the CONT** — accept it in-game", them)
	}
	return "CONT sent"
}

// tradeCard is a trade with its buttons, used for /trade and after any button press.
func (b *Bot) tradeCard(ctx context.Context, u *User, fillID int64, note string) botMessage {
	t, err := b.store.TradeByID(ctx, fillID, u.ID)
	if err != nil {
		if isUserError(err) {
			return botMessage{Content: note + err.Error()}
		}
		return botMessage{Content: note + "Something went wrong, try again in a moment."}
	}
	var buttons []any
	if t.Fulfilled {
		buttons = row(button(btnSecondary, "Undo", fmt.Sprintf("cont:%s:%d", ActUnfulfill, t.FillID)))
	} else {
		buttons = contButtons(t.Cont, t.FillID)
	}
	return botMessage{Content: note + tradeLine(t), Components: buttons}
}

// commands are the bot's slash commands, all usable in its DM.
func (b *Bot) commands() []any {
	cmd := func(name, desc string, options ...any) map[string]any {
		return map[string]any{
			"name": name, "description": desc, "type": 1, "options": options,
			"integration_types": []int{0, 1},    // guild install, user install
			"contexts":          []int{0, 1, 2}, // server, bot DM, any other DM
		}
	}
	opt := func(typ int, name, desc string, required bool, extra map[string]any) map[string]any {
		o := map[string]any{"type": typ, "name": name, "description": desc, "required": required}
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	currency := func(name, desc string, required bool) map[string]any {
		var choices []any
		for _, c := range Currencies {
			choices = append(choices, map[string]any{"name": c, "value": c})
		}
		return opt(3, name, desc, required, map[string]any{"choices": choices})
	}
	amount := func(required bool, desc string) map[string]any {
		return opt(4, "amount", desc, required, map[string]any{"min_value": 1})
	}
	orderID := func(desc string) map[string]any {
		return opt(4, "order", desc, true, map[string]any{"min_value": 1})
	}

	channel := opt(7, "channel", "Channel or thread to post the board in", true,
		map[string]any{"channel_types": []int{0, 5, 10, 11, 12}}) // text, announcement, threads
	// Boards live in a server, and only people who run it can set one up.
	board := cmd("board", "Keep an order board posted in a channel (server managers)",
		map[string]any{"type": 1, "name": "setup", "description": "Post and keep an order board in a channel", "options": []any{
			channel,
			opt(3, "mode", "When to update it", true, map[string]any{"choices": []any{
				map[string]any{"name": "when orders change", "value": "live"},
				map[string]any{"name": "on a schedule", "value": "every"},
			}}),
			opt(4, "every_minutes", "For a schedule: how often, in minutes (default 60)", false, map[string]any{"min_value": 5, "max_value": 1440}),
			opt(5, "delete_old", "For a schedule: remove the previous board each time", false, nil),
			opt(5, "mobile", "Plain list instead of the table, which phones wrap badly", false, nil),
			currency("from", "Only orders offering this currency", false),
			currency("to", "Only orders wanting this currency", false),
		}},
		map[string]any{"type": 1, "name": "stop", "description": "Stop updating the board in a channel", "options": []any{channel}},
		map[string]any{"type": 1, "name": "list", "description": "Show this server's order boards"})
	board["contexts"] = []int{0}               // servers only
	board["integration_types"] = []int{0}      // installed on the server
	board["default_member_permissions"] = "32" // Manage Server

	// Linking ties a Discord account to a company, so it stays in the bot's DM.
	link := cmd("link", "Link your Discord account and PrUn company so you can trade and get fill DMs",
		opt(3, "company_code", "Your Prosperous Universe company code, e.g. NIKU", true,
			map[string]any{"min_length": 1, "max_length": 4}))
	link["contexts"] = []int{1}

	return []any{
		link,
		cmd("unlink", "Stop PrUn Forex from DMing you (you'll need to /link again to trade)"),
		cmd("orders", "List open orders on the board",
			currency("from", "Only orders offering this currency", false),
			currency("to", "Only orders wanting this currency", false),
			opt(5, "mobile", "Plain list instead of the table, which phones wrap badly", false, nil),
			opt(5, "private", "Show the list only to you (default: everyone in the channel)", false, nil)),
		board,
		cmd("post", "Post an order to swap one currency for another, 1:1",
			amount(true, "How much you're offering"),
			currency("from", "What you have", true),
			currency("to", "What you want", true)),
		cmd("fill", "Fill someone else's order, fully or in part",
			orderID("Order number to fill, from /orders"),
			amount(false, "How much to fill (default: all of it)")),
		cmd("cancel", "Cancel one of your open orders",
			orderID("Your order number, from /myorders")),
		cmd("myorders", "List your own orders"),
		cmd("trades", "List your trades to settle",
			opt(5, "fulfilled", "Include trades you've marked fulfilled", false, nil)),
		cmd("trade", "Show one trade with its buttons",
			opt(4, "id", "Trade number, from /trades", true, map[string]any{"min_value": 1})),
	}
}

// requireUser resolves the Discord user to a linked trader.
func (b *Bot) requireUser(ctx context.Context, du discordUser) (*User, string) {
	u, err := b.store.UserByDiscordID(ctx, du.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, "Run `/link company_code:ABCD` first, with your in-game company code."
	case err != nil:
		return nil, "Something went wrong, try again in a moment."
	case !u.Ready():
		return nil, "Run `/link company_code:ABCD` again with your company code before trading."
	}
	return u, ""
}

// command runs a slash command other than /link and /unlink.
func (b *Bot) command(ctx context.Context, in interaction, du discordUser) botMessage {
	u, problem := b.requireUser(ctx, du)
	if problem != "" {
		return botMessage{Content: problem}
	}
	switch in.Data.Name {
	case "orders":
		return b.cmdOrders(ctx, u, in.option("from"), in.option("to"), in.optionBool("mobile"))
	case "post":
		return b.postPreview(ctx, u, in.option("from"), in.option("to"), in.optionInt("amount"), 0, true)
	case "fill":
		return b.cmdFill(ctx, u, in.optionInt("order"), in.optionInt("amount"))
	case "cancel":
		return b.cmdCancel(ctx, u, in.optionInt("order"))
	case "myorders":
		return b.cmdMyOrders(ctx, u)
	case "trades":
		return b.cmdTrades(ctx, u, in.optionBool("fulfilled"))
	case "trade":
		return b.tradeCard(ctx, u, in.optionInt("id"), "")
	}
	return botMessage{Content: "Unknown command."}
}

func (b *Bot) cmdOrders(ctx context.Context, u *User, from, to string, mobile bool) botMessage {
	f := OrderFilter{Status: "open", From: NormalizeCurrency(from), To: NormalizeCurrency(to), Limit: 20}
	orders, err := b.store.ListOrders(ctx, f)
	if err != nil {
		return oops(err)
	}
	msg := boardMessage(orders, f, mobile, "**Open orders**")
	if len(orders) > 0 {
		msg += "\nFill one with `/fill order:<number>`."
	} else {
		msg += " Post one with `/post`."
	}
	return botMessage{Content: msg}
}

func (b *Bot) cmdMyOrders(ctx context.Context, u *User) botMessage {
	orders, err := b.store.ListOrders(ctx, OrderFilter{UserID: u.ID, Limit: 15})
	if err != nil {
		return oops(err)
	}
	if len(orders) == 0 {
		return botMessage{Content: "You have no orders. Post one with `/post`."}
	}
	var sb strings.Builder
	sb.WriteString("**Your orders:**\n")
	for _, o := range orders {
		fmt.Fprintf(&sb, "`#%d` **%s → %s** — %s of %s filled — %s\n",
			o.ID, o.From, o.To, formatInt(o.Filled()), formatInt(o.Amount), o.Status)
	}
	sb.WriteString("\nCancel one with `/cancel order:<number>`.")
	return botMessage{Content: sb.String()}
}

func (b *Bot) cmdTrades(ctx context.Context, u *User, includeFulfilled bool) botMessage {
	trades, err := b.store.Trades(ctx, u.ID, includeFulfilled, 10)
	if err != nil {
		return oops(err)
	}
	if len(trades) == 0 {
		if includeFulfilled {
			return botMessage{Content: "You have no trades yet. Fill an order with `/fill`, or post one with `/post`."}
		}
		return botMessage{Content: "Nothing left to settle. Use `/trades fulfilled:True` to see finished ones."}
	}
	var sb strings.Builder
	sb.WriteString("**Your trades:**\n")
	var buttons []map[string]any
	for i, t := range trades {
		sb.WriteString(tradeLine(t) + "\n")
		if i < 5 {
			buttons = append(buttons, button(btnSecondary, fmt.Sprintf("Trade #%d", t.FillID), fmt.Sprintf("trade:%d", t.FillID)))
		}
	}
	sb.WriteString("\nPick one below, or use `/trade id:<number>`.")
	return botMessage{Content: sb.String(), Components: row(buttons...)}
}

func (b *Bot) cmdFill(ctx context.Context, u *User, orderID, amount int64) botMessage {
	n, fillID, err := b.store.Fill(ctx, orderID, u, amount)
	if err != nil {
		return oops(err)
	}
	return b.tradeCard(ctx, u, fillID, fmt.Sprintf("Filled **%s** on order `#%d`. The owner has been notified.\n", formatInt(n), orderID))
}

func (b *Bot) cmdCancel(ctx context.Context, u *User, orderID int64) botMessage {
	if err := b.store.Cancel(ctx, orderID, u.ID); err != nil {
		if isUserError(err) {
			return botMessage{Content: "Couldn't cancel that order: " + err.Error() + " Check `/myorders`."}
		}
		return oops(err)
	}
	return botMessage{Content: fmt.Sprintf("Order `#%d` cancelled. Fills so far still stand — see `/trades`.", orderID)}
}

// postPreview is /post: it checks for an order of the user's own on the same
// pair, then for orders it could fill, asking before doing either.
func (b *Bot) postPreview(ctx context.Context, u *User, from, to string, amount, into int64, checkDup bool) botMessage {
	from, to = NormalizeCurrency(from), NormalizeCurrency(to)
	if err := validateOrder(from, to, amount); err != nil {
		return botMessage{Content: err.Error()}
	}
	id := func(mode string, into int64) string {
		return fmt.Sprintf("post:%s:%s:%s:%d:%d", mode, from, to, amount, into)
	}
	if checkDup {
		existing, err := b.store.ExistingOrder(ctx, u.ID, from, to)
		if err != nil {
			return oops(err)
		}
		if existing != nil {
			return botMessage{
				Content: fmt.Sprintf("You already have an open **%s → %s** order `#%d` with **%s** open. Add **%s** to it, or post a separate order?",
					from, to, existing.ID, formatInt(existing.Remaining), formatInt(amount)),
				Components: row(
					button(btnPrimary, fmt.Sprintf("Add to #%d", existing.ID), id("post", existing.ID)),
					button(btnSecondary, "Post separately", id("post", 0))),
			}
		}
	}
	m, err := b.store.FindMatch(ctx, u.ID, from, to, amount)
	if err != nil {
		return oops(err)
	}
	if len(m.Legs) == 0 {
		return b.post(ctx, u, from, to, amount, false, into)
	}
	var sb strings.Builder
	if m.Full() {
		fmt.Fprintf(&sb, "Open **%s → %s** orders can fill your whole **%s %s** right now:\n", to, from, formatInt(amount), from)
	} else {
		fmt.Fprintf(&sb, "Open **%s → %s** orders cover **%s** of your **%s %s**, leaving **%s** unfilled:\n",
			to, from, formatInt(m.Available), formatInt(amount), from, formatInt(m.Leftover()))
	}
	for _, leg := range m.Legs {
		fmt.Fprintf(&sb, "`#%d` %s — you'd fill **%s**\n", leg.ID, leg.Owner.Name(), formatInt(leg.Take))
	}
	takeLabel := "Fill them instead"
	if !m.Full() {
		takeLabel = fmt.Sprintf("Fill %s, post the rest", formatInt(m.Available))
		if into != 0 {
			takeLabel = fmt.Sprintf("Fill %s, add the rest", formatInt(m.Available))
		}
	}
	postLabel := "Post it anyway"
	if into != 0 {
		postLabel = fmt.Sprintf("Add it all to #%d", into)
	}
	return botMessage{Content: sb.String(), Components: row(
		button(btnPrimary, takeLabel, id("take", into)),
		button(btnSecondary, postLabel, id("keep", into)))}
}

func (b *Bot) post(ctx context.Context, u *User, from, to string, amount int64, take bool, into int64) botMessage {
	res, err := b.store.PlaceOrder(ctx, u, from, to, amount, take, into)
	if err != nil {
		return oops(err)
	}
	var msg string
	if res.Filled > 0 {
		msg = fmt.Sprintf("Filled **%s %s → %s** from %d order(s). See `/trades`.", formatInt(res.Filled), from, to, res.Fills)
	}
	switch {
	case res.Order == nil:
	case res.Increased:
		msg += fmt.Sprintf(" Added **%s** to order `#%d`, now **%s** open.",
			formatInt(amount-res.Filled), res.Order.ID, formatInt(res.Order.Remaining))
	case res.Filled > 0:
		msg += fmt.Sprintf(" Posted `#%d` for the remaining **%s**.", res.Order.ID, formatInt(res.Order.Amount))
	default:
		msg = fmt.Sprintf("Order `#%d` posted: **%s %s → %s**.", res.Order.ID, formatInt(amount), from, to)
	}
	m := botMessage{Content: strings.TrimSpace(msg)}
	if res.Order != nil && res.Order.Status == "open" {
		m.Components = row(button(btnDanger, fmt.Sprintf("Cancel #%d", res.Order.ID), fmt.Sprintf("order:cancel:%d", res.Order.ID)))
	}
	return m
}

// buttonPress handles the bot's own buttons, other than /link's.
func (b *Bot) buttonPress(ctx context.Context, customID string, du discordUser) botMessage {
	u, problem := b.requireUser(ctx, du)
	if problem != "" {
		return botMessage{Content: problem}
	}
	kind, rest, _ := strings.Cut(customID, ":")
	switch kind {
	case "cont":
		action, idStr, _ := strings.Cut(rest, ":")
		fillID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return botMessage{Content: "That button is from an older message, use `/trades`."}
		}
		note := ""
		other, err := b.store.ContAct(ctx, fillID, u, ContAction(action))
		switch {
		case err != nil && !isUserError(err):
			return oops(err)
		case err != nil:
			note = err.Error() + "\n"
		case ContAction(action) == ActCancel:
			// The trade is gone now, so there's no card left to show.
			return botMessage{Content: fmt.Sprintf("Trade called off. **%s** gets a DM, and the amount is back on the order.", other.Name())}
		}
		return b.tradeCard(ctx, u, fillID, note)

	case "trade":
		fillID, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return botMessage{Content: "That button is from an older message, use `/trades`."}
		}
		return b.tradeCard(ctx, u, fillID, "")

	case "order":
		action, idStr, _ := strings.Cut(rest, ":")
		orderID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || action != "cancel" {
			return botMessage{Content: "That button is from an older message, use `/myorders`."}
		}
		return b.cmdCancel(ctx, u, orderID)

	case "post":
		// post:<mode>:<from>:<to>:<amount>:<into>
		parts := strings.Split(rest, ":")
		if len(parts) != 5 {
			return botMessage{Content: "That button is from an older message, use `/post`."}
		}
		amount, err1 := strconv.ParseInt(parts[3], 10, 64)
		into, err2 := strconv.ParseInt(parts[4], 10, 64)
		if err1 != nil || err2 != nil {
			return botMessage{Content: "That button is from an older message, use `/post`."}
		}
		from, to := parts[1], parts[2]
		switch parts[0] {
		case "post": // answered "add to my order" or "post separately": now check for matches
			return b.postPreview(ctx, u, from, to, amount, into, false)
		case "take":
			return b.post(ctx, u, from, to, amount, true, into)
		case "keep":
			return b.post(ctx, u, from, to, amount, false, into)
		}
	}
	return botMessage{Content: "That button doesn't do anything anymore."}
}

func oops(err error) botMessage {
	if isUserError(err) {
		return botMessage{Content: err.Error()}
	}
	return botMessage{Content: "Something went wrong, try again in a moment."}
}

func orDash(s string) string {
	if s == "" {
		return "any"
	}
	return s
}

// NormalizeCurrency uppercases a currency, or returns "" if it isn't one.
func NormalizeCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if !validCurrency(c) {
		return ""
	}
	return c
}
