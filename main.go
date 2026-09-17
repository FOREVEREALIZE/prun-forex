package main

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Config struct {
	Addr       string
	BaseURL    string
	DBPath     string
	AppID      string
	PublicKey  string
	BotToken   string
	InviteURL  string
	SessionKey []byte
	DevLogin   bool
}

func loadConfig() Config {
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Addr:      env("ADDR", ":8080"),
		BaseURL:   strings.TrimRight(env("BASE_URL", "http://localhost:8080"), "/"),
		DBPath:    env("DB_PATH", "forex.db"),
		AppID:     os.Getenv("DISCORD_APP_ID"),
		PublicKey: os.Getenv("DISCORD_PUBLIC_KEY"),
		BotToken:  os.Getenv("DISCORD_BOT_TOKEN"),
		InviteURL: os.Getenv("DISCORD_INVITE_URL"),
		DevLogin:  os.Getenv("DEV_LOGIN") == "1",
	}
	if k := os.Getenv("SESSION_KEY"); len(k) >= 32 {
		c.SessionKey = []byte(k)
	} else {
		if k != "" {
			log.Fatal("SESSION_KEY must be at least 32 characters")
		}
		log.Print("SESSION_KEY not set: using a random key, so sessions won't survive a restart")
		c.SessionKey = make([]byte, 32)
		rand.Read(c.SessionKey)
	}
	if c.AppID == "" && !c.DevLogin {
		log.Fatal("DISCORD_APP_ID is required (or set DEV_LOGIN=1 for local testing)")
	}
	return c
}

func main() {
	cfg := loadConfig()
	store, err := OpenStore(cfg.DBPath, cfg.BaseURL)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	bot, err := NewBot(cfg.AppID, cfg.BotToken, cfg.PublicKey, store)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.BotToken != "" && cfg.AppID != "" {
		if err := bot.RegisterCommands(ctx); err != nil {
			log.Printf("registering slash commands failed: %v", err)
		}
	} else {
		log.Print("DISCORD_BOT_TOKEN not set: DMs will only be logged")
	}
	go bot.RunOutbox(ctx)

	srv, err := NewServer(cfg, store, bot)
	if err != nil {
		log.Fatal(err)
	}
	hs := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	log.Printf("listening on %s (%s)", cfg.Addr, cfg.BaseURL)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
