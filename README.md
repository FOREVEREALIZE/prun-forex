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
| `FNAR_URL` | FIO REST API used to look up companies, default `https://rest.fnar.net` |
| `DISCORD_INVITE_URL` | Optional server invite shown as a fallback when DMs don't get through |
| `DEV_LOGIN=1` | **Local only**: enables `/dev/login?name=alice` (add `&unlinked=1` to test the link gate), so you don't need Discord |

## Discord app setup

1. **OAuth2 → Redirects**: add `$BASE_URL/auth/discord`. Sign-in uses the implicit grant in the browser, so there's no client secret. The server uses the access token once to call `/users/@me` and never stores it.
2. **Installation**: enable **User Install** and **Guild Install**, both with the `applications.commands` scope (Guild Install also wants `bot`). Users add the app to their account from the site's onboarding; server admins can invite it with `https://discord.com/oauth2/authorize?client_id=$DISCORD_APP_ID&scope=bot+applications.commands&permissions=0`.
3. **General → Interactions Endpoint URL**: `$BASE_URL/discord/interactions`. For local dev, expose the server with `cloudflared tunnel --url localhost:8080`.
4. **Bot**: copy the token into `DISCORD_BOT_TOKEN`.

Settling a trade: under **Trades to settle**, one side volunteers to send the CONT, or asks the other, who can accept or ask back (each request DMs the other side). The sender marks the CONT sent, then each side marks the trade fulfilled, which hides it for them. Tick **Show fulfilled** to see those again.

Before a user can trade, they add the app, DM the bot and run `/link company_code:ABCD` with their in-game company code. The bot looks the company up on FNAR, shows who owns it and which corporation it's in, and asks the user to confirm with Yes/No. On Yes it stores the company code, the in-game username and the corporation code. Traders then show as `[CORP] Username | CODE` with their `@discord` handle underneath. Each company can only be linked to one Discord account. Users who linked before company codes existed are sent back through onboarding on the site, with an explanation, until they run `/link` again. If a DM later fails with "cannot send messages to this user", they're unlinked and have to run `/link` again.

## Discord commands

Every command except `/link`, which stays in the bot's DM, works in the bot's DM, in a server, and in any other DM, whether the app is installed on a server or added to your own account. `/orders` posts where everyone can see it (the board is public on the site too, and `private:True` keeps it to yourself); everything else — your orders, trades, fills and linking — is shown only to whoever ran it. In the bot's own DM nothing is hidden, so the messages stay in your history.

The DMs the bot sends carry buttons for the next step (accept a CONT request, mark it sent, mark the trade fulfilled, call it off). Pressing a button rewrites that message as an up-to-date card for the trade.

| command | |
|---|---|
| `/link company_code:ABCD` | Link your Discord account and company (DM only) |
| `/unlink` | Stop the DMs (you'll need to `/link` again to trade) |
| `/orders [from] [to] [mobile] [private]` | Open orders, as a coloured table (`mobile:True` gives a plain list for phones) |
| `/post amount from to` | Post an order; asks first if it could fill existing orders, or if you already have one for that pair |
| `/fill order [amount]` | Fill someone's order, fully or in part |
| `/cancel order` | Cancel your own order |
| `/myorders` | Your orders |
| `/trades [fulfilled]` | Your trades to settle, with buttons |
| `/trade id` | One trade with its buttons |
| `/board setup channel mode …` | Keep an order board posted in a channel (server managers) |
| `/board stop channel` · `/board list` | Stop one, or list this server's boards |

### Order boards in a channel

`/board setup` posts the board in a channel or thread and keeps it there. It needs Manage Server, and the bot has to be in the server (invite link above).

- **`mode: when orders change`** — a fill is edited into the message where it is; a new order reposts the board and removes the old message, so it lands at the bottom of the channel.
- **`mode: on a schedule`** — reposts every `every_minutes` minutes (5–1440, default 60), with `delete_old:True` to remove the previous board each time.
- `from` / `to` limit it to one pair, and `mobile:True` posts the plain list instead of the table.

Setting one up posts immediately, so a channel the bot can't write to fails there and then rather than quietly. A board that errors five times in a row is left alone.

The bot's status reads **Watching 2.5m posted orders** — the total still unfilled across every open order. That needs a gateway connection, which the bot opens when `DISCORD_BOT_TOKEN` is set; it subscribes to no events and needs no privileged intents.

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
