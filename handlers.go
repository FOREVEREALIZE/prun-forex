package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg   Config
	store *Store
	bot   *Bot
	tmpl  *template.Template
}

func NewServer(cfg Config, store *Store, bot *Bot) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"ago":  ago,
		"num":  formatInt,
		"add":  func(a, b int64) int64 { return a + b },
		"pair": func(p string) string { return strings.Replace(p, "-", " → ", 1) },
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: store, bot: bot, tmpl: tmpl}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /fragments/live", s.handleLive)
	mux.HandleFunc("GET /link/status", s.handleLinkStatus)

	mux.HandleFunc("GET /auth/discord", s.handleAuthPage)
	mux.HandleFunc("POST /auth/session", s.handleAuthSession)
	mux.HandleFunc("POST /logout", s.handleLogout)
	if s.cfg.DevLogin {
		log.Print("DEV_LOGIN=1: /dev/login?name=... is enabled, never use this in production")
		mux.HandleFunc("GET /dev/login", s.handleDevLogin)
	}

	mux.HandleFunc("POST /orders/preview", s.action(s.handlePreview))
	mux.HandleFunc("POST /orders", s.action(s.handlePlace))
	mux.HandleFunc("POST /orders/{id}/fill", s.action(s.handleFill))
	mux.HandleFunc("POST /orders/{id}/cancel", s.action(s.handleCancel))

	mux.HandleFunc("GET /api/orders", s.handleAPIOrders)
	mux.HandleFunc("GET /api/orders/{id}", s.handleAPIOrder)

	mux.HandleFunc("POST /discord/interactions", s.bot.HandleInteraction)

	return securityHeaders(s.withUser(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// action wraps state-changing htmx endpoints: requires the htmx header (which
// a cross-site form can't send) and a signed-in, linked user.
func (s *Server) action(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("HX-Request") != "true" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		u := currentUser(r)
		switch {
		case u == nil:
			s.toast(w, "err", "Sign in with Discord first.")
		case !u.Linked:
			w.Header().Set("HX-Refresh", "true")
		default:
			h(w, r, u)
		}
	}
}

type PageData struct {
	User       *User
	LoginURL   string
	InstallURL string
	BotDMURL   string
	InviteURL  string
	DevLogin   bool
	Currencies []string
	Live       *LiveData
}

type LiveData struct {
	User      *User
	Pair      string
	Pairs     []string
	Board     []Order
	MyOrders  []Order
	Trades    []Trade
	UpdatedAt time.Time
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d := PageData{
		User:       u,
		LoginURL:   s.loginURL(),
		InstallURL: "https://discord.com/oauth2/authorize?client_id=" + url.QueryEscape(s.cfg.AppID) + "&integration_type=1&scope=applications.commands",
		BotDMURL:   "https://discord.com/users/" + url.PathEscape(s.cfg.AppID),
		InviteURL:  s.cfg.InviteURL,
		DevLogin:   s.cfg.DevLogin,
		Currencies: Currencies,
	}
	if u == nil || u.Linked {
		live, err := s.liveData(r, u)
		if err != nil {
			serverError(w, err)
			return
		}
		d.Live = live
	}
	s.render(w, "page", d)
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	live, err := s.liveData(r, currentUser(r))
	if err != nil {
		serverError(w, err)
		return
	}
	s.render(w, "live", live)
}

func (s *Server) liveData(r *http.Request, u *User) (*LiveData, error) {
	ctx := r.Context()
	d := &LiveData{User: u, UpdatedAt: time.Now()}
	for _, a := range Currencies {
		for _, b := range Currencies {
			if a != b {
				d.Pairs = append(d.Pairs, a+"-"+b)
			}
		}
	}
	f := OrderFilter{Status: "open"}
	if from, to, ok := strings.Cut(r.URL.Query().Get("pair"), "-"); ok && validCurrency(from) && validCurrency(to) {
		f.From, f.To = from, to
		d.Pair = from + "-" + to
	}
	var err error
	if d.Board, err = s.store.ListOrders(ctx, f); err != nil {
		return nil, err
	}
	if u != nil && u.Linked {
		if d.MyOrders, err = s.store.ListOrders(ctx, OrderFilter{UserID: u.ID, Limit: 30}); err != nil {
			return nil, err
		}
		if d.Trades, err = s.store.Trades(ctx, u.ID, 30); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (s *Server) handleLinkStatus(w http.ResponseWriter, r *http.Request) {
	if u := currentUser(r); u == nil || u.Linked {
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseOrderForm(r *http.Request) (from, to string, amount int64, err error) {
	from, to = r.FormValue("from"), r.FormValue("to")
	amount, perr := strconv.ParseInt(strings.TrimSpace(r.FormValue("amount")), 10, 64)
	if perr != nil {
		return from, to, 0, userError("Amount must be a whole number.")
	}
	return from, to, amount, validateOrder(from, to, amount)
}

type DupModal struct {
	From, To string
	Amount   int64
	Existing *Order
}

type MatchModal struct {
	Match
	Into *Order // the user's existing order to add the leftover to, if chosen
}

// handlePreview runs the checks before posting an order, each of which can
// stop with a popup: first "you already have an order for this pair" (unless
// dup was already answered), then "other orders can fill this right away".
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request, u *User) {
	ctx := r.Context()
	from, to, amount, err := parseOrderForm(r)
	if err != nil {
		s.toast(w, "err", err.Error())
		return
	}

	var into *Order
	switch dup := r.FormValue("dup"); dup {
	case "":
		existing, err := s.store.ExistingOrder(ctx, u.ID, from, to)
		if err != nil {
			serverError(w, err)
			return
		}
		if existing != nil {
			s.render(w, "dup-modal", DupModal{From: from, To: to, Amount: amount, Existing: existing})
			return
		}
	case "new":
	default:
		if into, err = s.ownOpenOrder(r, u, dup, from, to); err != nil {
			s.toast(w, "err", err.Error())
			return
		}
	}

	m, err := s.store.FindMatch(ctx, u.ID, from, to, amount)
	if err != nil {
		serverError(w, err)
		return
	}
	if len(m.Legs) > 0 {
		s.render(w, "modal", MatchModal{Match: m, Into: into})
		return
	}
	s.place(w, r, u, from, to, amount, false, into)
}

func (s *Server) ownOpenOrder(r *http.Request, u *User, idStr, from, to string) (*Order, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, ErrNotFound
	}
	o, err := s.store.OrderByID(r.Context(), id)
	switch {
	case err != nil:
		return nil, ErrNotFound
	case o.UserID != u.ID || o.From != from || o.To != to:
		return nil, ErrNotFound
	case o.Status != "open":
		return nil, ErrClosed
	}
	return o, nil
}

func (s *Server) handlePlace(w http.ResponseWriter, r *http.Request, u *User) {
	from, to, amount, err := parseOrderForm(r)
	if err != nil {
		s.toast(w, "err", err.Error())
		return
	}
	var into *Order
	if v := r.FormValue("into"); v != "" {
		if into, err = s.ownOpenOrder(r, u, v, from, to); err != nil {
			s.toast(w, "err", err.Error())
			return
		}
	}
	s.place(w, r, u, from, to, amount, r.FormValue("mode") == "take", into)
}

func (s *Server) place(w http.ResponseWriter, r *http.Request, u *User, from, to string, amount int64, take bool, into *Order) {
	var intoID int64
	if into != nil {
		intoID = into.ID
	}
	res, err := s.store.PlaceOrder(r.Context(), u, from, to, amount, take, intoID)
	if err != nil {
		if isUserError(err) {
			s.toast(w, "err", err.Error())
		} else {
			serverError(w, err)
		}
		return
	}
	var msg string
	if res.Filled > 0 {
		msg = fmt.Sprintf("Filled %s %s → %s from %d order(s).", formatInt(res.Filled), from, to, res.Fills)
	}
	switch {
	case res.Order == nil:
		msg += " Check your trades."
	case res.Increased:
		msg += fmt.Sprintf(" Added %s to order #%d, now %s open.",
			formatInt(amount-res.Filled), res.Order.ID, formatInt(res.Order.Remaining))
	case res.Filled > 0:
		msg += fmt.Sprintf(" Posted #%d for the remaining %s.", res.Order.ID, formatInt(res.Order.Amount))
	default:
		msg = fmt.Sprintf("Order #%d posted: %s %s → %s.", res.Order.ID, formatInt(amount), from, to)
	}
	w.Header().Set("HX-Trigger", "refresh, orderPlaced")
	s.toast(w, "ok", strings.TrimSpace(msg))
}

func (s *Server) handleFill(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var amount int64
	if v := strings.TrimSpace(r.FormValue("amount")); v != "" {
		if amount, err = strconv.ParseInt(v, 10, 64); err != nil || amount <= 0 {
			s.toast(w, "err", "Amount must be a positive whole number.")
			return
		}
	}
	n, err := s.store.Fill(r.Context(), id, u, amount)
	w.Header().Set("HX-Trigger", "refresh")
	if err != nil {
		if isUserError(err) {
			s.toast(w, "err", err.Error())
		} else {
			serverError(w, err)
		}
		return
	}
	s.toast(w, "ok", fmt.Sprintf("Filled %s on order #%d. The owner has been notified.", formatInt(n), id))
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("HX-Trigger", "refresh")
	if err := s.store.Cancel(r.Context(), id, u.ID); err != nil {
		if isUserError(err) {
			s.toast(w, "err", err.Error())
		} else {
			serverError(w, err)
		}
		return
	}
	s.toast(w, "ok", fmt.Sprintf("Order #%d cancelled.", id))
}

func (s *Server) toast(w http.ResponseWriter, kind, msg string) {
	s.render(w, "toast", map[string]string{"Kind": kind, "Msg": msg})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func formatInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
