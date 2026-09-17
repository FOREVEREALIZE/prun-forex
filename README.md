# PrUn Forex

A small order board for swapping Prosperous Universe currencies (AIC / CIS / ICA / NCC) 1:1 in whole units. It's one Go binary backed by SQLite, the pages are rendered on the server with htmx, and a Discord bot DMs people when their orders get filled. Players settle the actual trades in-game.

## Run

```sh
go build -o prun-forex .
DISCORD_APP_ID=... DISCORD_PUBLIC_KEY=... DISCORD_BOT_TOKEN=... \
SESSION_KEY=$(openssl rand -hex 32) BASE_URL=https://forex.example.com \
./prun-forex
```

| env | |
|---|---|
| `DISCORD_APP_ID` | Application ID (also the OAuth client ID) |
| `DISCORD_PUBLIC_KEY` | Used to verify slash-command interactions |
| `DISCORD_BOT_TOKEN` | Used to send DMs and register `/link` + `/unlink` at startup. Without it, DMs are only logged |
| `SESSION_KEY` | ≥32 chars, signs the session cookie |
| `BASE_URL` | Public URL, default `http://localhost:8080` |
| `ADDR` / `DB_PATH` | Default `:8080` / `forex.db` |
| `DISCORD_INVITE_URL` | Optional server invite shown as a fallback when DMs don't get through |
| `DEV_LOGIN=1` | **Local only**: enables `/dev/login?name=alice` (add `&unlinked=1` to test the link gate), so you don't need Discord |

## Discord app setup

1. **OAuth2 → Redirects**: add `$BASE_URL/auth/discord`. Sign-in uses the implicit grant in the browser, so there's no client secret. The server uses the access token once to call `/users/@me` and never stores it.
2. **Installation**: enable **User Install** (and Guild Install if you want) with the `applications.commands` scope.
3. **General → Interactions Endpoint URL**: `$BASE_URL/discord/interactions`. For local dev, expose the server with `cloudflared tunnel --url localhost:8080`.
4. **Bot**: copy the token into `DISCORD_BOT_TOKEN`.

Before a user can trade, they add the app, DM the bot and run `/link`. If a DM later fails with "cannot send messages to this user", they're unlinked and have to run `/link` again.

## JSON API

- `GET /api/orders?status=open|filled|cancelled|all&from=AIC&to=NCC&limit=100`
- `GET /api/orders/{id}` (includes fills)

## Tests

```sh
go test -race ./...
```

## Docker

```sh
cp .env.example .env   # fill it in
docker compose up -d
```

The SQLite database lives in the `forex-data` volume at `/data/forex.db`. CI publishes multi-arch images to `ghcr.io/foreverealize/prun-forex` on pushes to `main` and on `v*` tags.
