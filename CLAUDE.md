Pitchside is a live match companion for the World Cup. A fan subscribes to a match and gets live alerts on every goal, corner, and card, plus a "bottle watch" when their favourite team blows a lead. Data is the TxLINE scores feed on Solana devnet, streamed through the existing Go proxy. SportyBet-inspired look: dark theme, red primary. Keep all existing data adapters, the SSE parser, the stat encoding, and the event bus untouched. This product spec overrides the older corner-game description.

# Touchline, build brief for Claude Code

Touchline is a live higher-or-lower streak game for the World Cup. The player calls
who wins the next corner, builds a streak, and shares it. Data comes from the TxLINE
sports feed (a Superteam / TxODDS hackathon). The frontend already exists and works on
a simulated match. The job now is to make it run on real TxLINE data, end to end, on
Solana devnet, with everything free.

Read this whole file before writing code. The constants below are verified from the
TxLINE docs. Do not invent addresses, seeds, or account names. If something here
conflicts with your prior knowledge of Solana token programs, this file wins.

## Hard rules

- No Tailwind. Hand-written CSS only. The existing frontend uses custom CSS, keep it.
- No em-dashes anywhere, in code, comments, copy, or docs. Use commas, colons,
  parentheses, or hyphens.
- Do not change the game logic or visual design. Only swap the data layer.
- Never commit the wallet keypair or any token. They go in files that are gitignored.
- The TxL mint is a Token-2022 mint. Use TOKEN_2022_PROGRAM_ID everywhere a token
  program or ATA derivation is involved. Do not use the classic TOKEN_PROGRAM_ID.

## Architecture (three parts)

1. scripts/  A one-time Node + Anchor script that runs the on-chain handshake on
   devnet and prints a long-lived API token. Run it once, copy the token, done.
2. proxy/    A small Go service. It holds the API token, opens the upstream TxLINE
   SSE stream, and re-streams it to the browser so the token never reaches the client.
   It also serves the static frontend and proxies the historical endpoint.
3. web/      The existing frontend. Add two real data adapters that talk to the proxy
   (live SSE and historical replay), keeping the simulated one as an offline fallback.

The browser only ever talks to the proxy on the same origin. The token lives only in
the proxy environment.

## Verified devnet constants

```
RPC URL              https://api.devnet.solana.com
Program ID           6pW64gN1s2uqjHkn1unFeEjAwJkPGHoppGvS715wyP2J
TxL token mint       4Zao8ocPhmMgq7PdsYWyxvqySMGx7xb9cMftPMkEokRG
API base             https://txline-dev.txodds.com
Token program        TOKEN_2022_PROGRAM_ID  (from @solana/spl-token)
IDL program name     txoracle  (version 1.5.2)
```

Free service levels (no token cost, no USDT, nothing transferred):
```
SERVICE_LEVEL_ID = 12   World Cup + Int Friendlies, real-time   (use this)
SERVICE_LEVEL_ID = 1    World Cup + Int Friendlies, 60s delay   (fallback)
DURATION_WEEKS   = 4    must be a multiple of 4, the program rejects other values
SELECTED_LEAGUES = []   empty array means the standard bundle
```

## The subscribe instruction (exact wiring)

The IDL `subscribe` instruction takes args `serviceLevelId: u16` and `weeks: u8`, with
these accounts. Derive the PDAs and ATAs exactly like this:

```
programId        = new PublicKey("6pW64gN1s2uqjHkn1unFeEjAwJkPGHoppGvS715wyP2J")
txlMint          = new PublicKey("4Zao8ocPhmMgq7PdsYWyxvqySMGx7xb9cMftPMkEokRG")

[tokenTreasuryPda] = PublicKey.findProgramAddressSync(
    [Buffer.from("token_treasury_v2")], programId)

tokenTreasuryVault = getAssociatedTokenAddressSync(
    txlMint, tokenTreasuryPda, true, TOKEN_2022_PROGRAM_ID)

[pricingMatrixPda] = PublicKey.findProgramAddressSync(
    [Buffer.from("pricing_matrix")], programId)

userTokenAccount   = getAssociatedTokenAddressSync(
    txlMint, user.publicKey, false, TOKEN_2022_PROGRAM_ID)
```

Account names passed to `.accounts({ ... })` (camelCase IDL):
```
user                  -> wallet public key (signer)
pricingMatrix         -> pricingMatrixPda
tokenMint             -> txlMint
userTokenAccount      -> userTokenAccount (create the ATA first if it does not exist)
tokenTreasuryVault    -> tokenTreasuryVault
tokenTreasuryPda      -> tokenTreasuryPda
tokenProgram          -> TOKEN_2022_PROGRAM_ID
systemProgram         -> SystemProgram.programId
associatedTokenProgram-> ASSOCIATED_TOKEN_PROGRAM_ID
```

Call: `program.methods.subscribe(12, 4).accounts({ ... }).rpc()`.

If the program returns error 6016 (activeSubscription), that is fine, a subscription
already exists. Skip straight to activation and reuse it. If it returns 6041
(InvalidWeeks), weeks was not a multiple of 4.

## Activation (turns the on-chain subscription into an API token)

After `subscribe` succeeds (or already exists), do this against the devnet host:

```
1. POST https://txline-dev.txodds.com/auth/guest/start
   -> returns { token: jwt }   (JWT is valid 30 days)

2. message = `${txSig}:${SELECTED_LEAGUES.join(",")}:${jwt}`
   sign the UTF-8 bytes with the wallet secret key using nacl.sign.detached
   walletSignature = base64(signatureBytes)

3. POST https://txline-dev.txodds.com/api/token/activate
   headers: Authorization: Bearer ${jwt}
   body:    { txSig, walletSignature, leagues: SELECTED_LEAGUES }
   -> returns the apiToken (use activationResponse.data.token || activationResponse.data)
```

If `/auth/guest/start` 404s on the devnet host, try the same path on
https://txline.txodds.com for the guest JWT only, but keep activation and all data
calls on the devnet host, since the subscription lives on devnet.

Print both jwt and apiToken at the end. These two strings are all the proxy needs.

## Proxy contract (Go)

Environment: TXLINE_JWT, TXLINE_API_TOKEN, TXLINE_BASE (default https://txline-dev.txodds.com),
PORT (default 8080).

Routes:
```
GET /api/stream/scores
    Open TXLINE_BASE + /api/scores/stream with headers:
      Authorization: Bearer <TXLINE_JWT>
      X-Api-Token:  <TXLINE_API_TOKEN>
      Accept: text/event-stream
      Cache-Control: no-cache
    Stream the response body to the client unbuffered. Set the client response
    headers to text/event-stream, no-cache, keep-alive, and flush after every write.

GET /api/scores/historical/{fixtureId}
    Proxy a GET to TXLINE_BASE + /api/scores/historical/{fixtureId} with the same
    Authorization and X-Api-Token headers. Return the JSON as-is.

GET /  and static
    Serve the ./web directory. index.html at root.
```

Keep it one file (main.go) plus go.mod. No external deps beyond the standard library.
Use http.Flusher for the stream route. Do not log the token.

## Frontend adapter contract (web/)

The game reads a normalised event bus with events: "match", "state", "tick", "corner".
There is already a connectDemo(bus, fixture) that drives a synthetic match, and a
connectLive(bus, fixture, cfg) stub. Do the following without touching game logic:

- Point connectLive at the same-origin proxy route /api/stream/scores (no token in the
  browser). Parse SSE lines, JSON.parse the data payload, and map the TxLINE soccer
  stat encoding into the existing internal events.
- Add connectReplay(bus, fixture, { fixtureId }) that GETs /api/scores/historical/{id},
  then replays the array in order with a short delay between updates so the game plays
  exactly as if live. This is what the demo video uses.
- Add a small mode switch (demo / replay / live) so the page can run offline for
  development and on real data for the recording and a live match.

### TxLINE soccer stat encoding

A score update carries stats keyed by `(period * 1000) + baseKey`. Strip the period
with `key % 1000` to get the base stat. Participant 1 is home, participant 2 is away.

```
base 1, 2   goals (home, away)
base 3, 4   yellow cards (home, away)
base 5, 6   red cards (home, away)
base 7, 8   corners (home, away)
```

Game phase ids: 1 not started, 2 first half, 3 half time, 4 second half, 5 full time,
and higher ids for extra time and penalties. A corner is resolved when base 7 or base 8
increases versus the previous value, which tells you which side won the corner.

The live payload field names beyond seq, ts, and gameState are not fully documented.
On the first real event, log the raw JSON, confirm where the stat array lives and its
field names, and adjust the parser. Everything downstream stays the same.

## Definition of done

- `scripts` prints a working apiToken on devnet.
- `proxy` serves the frontend and streams real scores through /api/stream/scores.
- The frontend plays a real match in replay mode end to end.
- Nothing secret is committed. README explains run and deploy in a few steps.
