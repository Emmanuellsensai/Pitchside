# Pitchside

A live match companion for the World Cup. Subscribe to a match and follow every moment as it happens: goals, corners, and cards animate on a live pitch, a momentum meter tracks the swing of play, and a Fan Zone lets supporters chat and call the action together in real time. Built on the TxLINE sports data feed over Solana.

Pitchside is built for the fan watching on their phone, the experience that until now only large operators could offer, made into something anyone can open and enjoy.

## What it does

- **Live match view.** A SportyBet-inspired pitch reacts to real events: the ball drives into the net on a goal, a corner flag raises on a corner, and cards appear on a booking, all driven by the live TxLINE feed.
- **Live alerts.** A running feed of every goal, corner, and card with the minute and team, with a mute toggle.
- **Momentum and bottle watch.** A live pressure meter that leans toward the team on top and decays back when play settles, plus a bottle-risk indicator that rises when your chosen team holds a lead and then surrenders it.
- **Fan Zone.** A real-time room per match where fans chat, send reactions, and cast no-stakes predictions (match winner and corners), with live sentiment bars showing how the room is calling it.
- **Match list and replay.** Browse live and finished matches, search by team, and replay any finished match end to end in a couple of minutes.

## How it works

Pitchside reads the TxLINE scores feed for live and historical match data. Access is granted through a one-time Solana subscription, which mints an API token used to stream data. A small Go service holds that token, streams the data to the browser over Server-Sent Events, and hosts the frontend, so the token never reaches the client. The Fan Zone runs over WebSockets, with one in-memory room per match.

```
Solana devnet  ->  subscription + API token
TxLINE feed    ->  Go proxy (token held server-side, SSE relay + WebSocket Fan Zone)
Go proxy       ->  browser (live pitch, alerts, Fan Zone)
```

## Tech stack

- **Frontend:** vanilla HTML, CSS, and JavaScript, mobile-first, no framework.
- **Backend:** Go (standard library plus coder/websocket), acting as a token proxy, SSE relay, static host, and Fan Zone hub.
- **Data:** TxLINE sports feed (scores, fixtures).
- **Chain:** Solana devnet, used for the data subscription.

## TxLINE endpoints used

- `POST /auth/guest/start` — guest authentication
- `POST /api/token/activate` — activate the API token after subscribing on-chain
- `GET /api/scores/stream` — live score and event stream (SSE)
- `GET /api/scores/historical/{fixtureId}` — full event history for replay
- Fixtures snapshot — live and upcoming match list

## Running locally

Requirements: Go 1.22+, Node 18+ (for the one-time subscription script), and a Solana wallet on devnet.

1. **Get an API token.** Run the subscription script once to subscribe on devnet and print a JWT and API token:
   ```
   cd scripts && npm install && npm run subscribe
   ```
2. **Configure the proxy.** Put the printed values in `proxy/.env`:
   ```
   TXLINE_JWT=...
   TXLINE_API_TOKEN=...
   TXLINE_BASE=https://txline-dev.txodds.com
   PORT=8080
   ```
3. **Run it.**
   ```
   cd proxy && go run .
   ```
4. Open `http://localhost:8080` and pick a match, or replay a finished one.

The subscription lasts four weeks and the JWT thirty days, so step 1 is a one-time setup.

## Project structure

```
pitchside/
  web/        frontend (single-page app)
  proxy/      Go service: token proxy, SSE relay, static host, Fan Zone hub
  scripts/    one-time Solana subscription and fixtures helper
```

## Notes

The prediction features are fan sentiment only, with no stakes and no money involved. Pitchside is an information and fan-engagement product, not a betting service.

Built for the TxODDS World Cup hackathon on Superteam Earn.
