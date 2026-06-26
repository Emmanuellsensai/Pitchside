# Touchline

A live higher-or-lower streak game for the World Cup. Call who wins the next corner,
build a streak, share it. Data comes from the TxLINE sports feed on Solana devnet.

The project has three parts:

- `scripts/` A one-time Node + Anchor handshake that runs on devnet and prints a
  long-lived API token.
- `proxy/` A small Go service that holds the token, opens the upstream TxLINE stream,
  and re-streams it to the browser. It also serves the frontend.
- `web/` The existing frontend with live, replay, and demo data adapters.

The browser only ever talks to the proxy on the same origin. The token lives only in
the proxy environment. Nothing secret is committed (see `.gitignore`).

## 1. One-time setup: get the API token

The handshake lives in `scripts/`. It loads the wallet from `dev-wallet.json`, loads
the IDL from `idl/txoracle.json`, connects to devnet, runs the `subscribe` instruction
(service level 12, 4 weeks), then activates and prints the JWT and API token.

### Prerequisites

A funded devnet wallet at `scripts/dev-wallet.json`:

```bash
solana-keygen new --no-bip39-passphrase --outfile scripts/dev-wallet.json
solana config set --url https://api.devnet.solana.com
solana airdrop 2 $(solana-keygen pubkey scripts/dev-wallet.json)
solana balance $(solana-keygen pubkey scripts/dev-wallet.json)
```

If the airdrop is rate limited, retry or use the web faucet at https://faucet.solana.com.

### Run the handshake

```bash
cd scripts
npm install
npm run subscribe
```

This prints two lines at the end:

```
TXLINE_JWT=...
TXLINE_API_TOKEN=...
```

Copy both into the proxy environment (see below). The JWT is valid 30 days.

Notes:
- If the program returns error 6016 (ActiveSubscription), that is fine. The script
  reuses the existing subscription and proceeds straight to activation.
- Error 6041 (InvalidWeeks) means weeks was not a multiple of 4.
- The TxL mint is a Token-2022 mint, so the script creates the user associated token
  account with `TOKEN_2022_PROGRAM_ID` before subscribing if it does not exist.

## 2. Run the proxy and frontend

The Go proxy holds the token, streams scores, and serves the frontend. It is
standard library only, so there is nothing to `go get`. Put the tokens in
`proxy/.env` (gitignored), which the proxy loads automatically on start:

```bash
cd proxy
cp .env.example .env
# edit .env, paste the TXLINE_JWT and TXLINE_API_TOKEN from the handshake
go run .            # serves on :8080 by default
```

Environment variables, if exported, override `.env`:

```
TXLINE_JWT          required, the guest JWT
TXLINE_API_TOKEN    required, the activated API token
TXLINE_BASE         default https://txline-dev.txodds.com
PORT                default 8080
WEB_DIR             default ../web
```

Routes:

```
GET /api/stream/scores              SSE relay of the upstream scores stream
GET /api/scores/historical/{id}     JSON proxy of one fixture's history
GET /  and static                   serves ../web, frontend at root
```

Open http://localhost:8080 and use the mode switch (demo / replay / live):

- demo:   the offline simulated match, no proxy or token needed.
- replay: plays a real fixture from history. Supply the fixture with
          `?fixtureId=<id>` in the URL (otherwise the page asks for one).
- live:   streams the real-time scores feed through the proxy.

## Security

- `scripts/dev-wallet.json` and any `.env` are gitignored. Never commit them.
- The API token never reaches the browser. It is only set in the proxy environment.
