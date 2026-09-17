package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "forex_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type ctxKey struct{}

// sessionValue is "userID.expiryUnix.signature": our own session, not a Discord token.
func (s *Server) sessionValue(userID int64, exp time.Time) string {
	payload := fmt.Sprintf("%d.%d", userID, exp.Unix())
	return payload + "." + s.sign(payload)
}

func (s *Server) sign(payload string) string {
	m := hmac.New(sha256.New, s.cfg.SessionKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) parseSession(v string) (int64, bool) {
	i := strings.LastIndexByte(v, '.')
	if i < 0 {
		return 0, false
	}
	payload, sig := v[:i], v[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return 0, false
	}
	idStr, expStr, ok := strings.Cut(payload, ".")
	if !ok {
		return 0, false
	}
	id, err1 := strconv.ParseInt(idStr, 10, 64)
	exp, err2 := strconv.ParseInt(expStr, 10, 64)
	if err1 != nil || err2 != nil || time.Now().Unix() > exp {
		return 0, false
	}
	return id, true
}

func (s *Server) setSession(w http.ResponseWriter, userID int64) {
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.sessionValue(userID, exp),
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// withUser attaches the signed-in user (if any) to the request context.
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if id, ok := s.parseSession(c.Value); ok {
				if u, err := s.store.UserByID(r.Context(), id); err == nil {
					r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, u))
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(ctxKey{}).(*User)
	return u
}

// loginURL is Discord's implicit-grant authorize URL. The browser adds a
// random state before redirecting (see app.js).
func (s *Server) loginURL() string {
	q := url.Values{
		"client_id":     {s.cfg.AppID},
		"response_type": {"token"},
		"redirect_uri":  {s.cfg.BaseURL + "/auth/discord"},
		"scope":         {"identify"},
		"prompt":        {"none"},
	}
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}

func (s *Server) handleAuthPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "auth", nil)
}

// handleAuthSession takes the access token the browser got from Discord, uses
// it once to find out who the user is, and forgets it.
func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") != "fetch" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.AccessToken == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	du, err := fetchDiscordUser(r.Context(), body.AccessToken)
	if err != nil {
		log.Printf("auth: discord identify failed: %v", err)
		http.Error(w, "couldn't verify your Discord login", http.StatusUnauthorized)
		return
	}
	u, err := s.store.UpsertUser(r.Context(), du.ID, du.displayName(), du.Avatar, false)
	if err != nil {
		serverError(w, err)
		return
	}
	s.setSession(w, u.ID)
	w.WriteHeader(http.StatusNoContent)
}

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Avatar     string `json:"avatar"`
}

func (d discordUser) displayName() string {
	if d.GlobalName != "" {
		return d.GlobalName
	}
	return d.Username
}

var discordHTTP = &http.Client{Timeout: 10 * time.Second}

func fetchDiscordUser(ctx context.Context, token string) (discordUser, error) {
	var du discordUser
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discordAPI+"/users/@me", nil)
	if err != nil {
		return du, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := discordHTTP.Do(req)
	if err != nil {
		return du, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return du, fmt.Errorf("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&du); err != nil {
		return du, err
	}
	if du.ID == "" {
		return du, errors.New("no user id")
	}
	return du, nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusNoContent)
}

// handleDevLogin signs in as a fake, already-linked user. Only with DEV_LOGIN=1.
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" || len(name) > 32 {
		http.Error(w, "?name= required", http.StatusBadRequest)
		return
	}
	u, err := s.store.UpsertUser(r.Context(), "dev-"+name, name, "", r.URL.Query().Get("unlinked") == "")
	if err != nil {
		serverError(w, err)
		return
	}
	s.setSession(w, u.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func isDevID(discordID string) bool { return strings.HasPrefix(discordID, "dev-") }
