// Fan Zone: a live chat plus a fan-sentiment prediction board, one room per
// fixtureId. State is held in memory only (no database) and resets on restart.
//
// The predictions are sentiment, picks with no stakes and no money, so this stays
// clear of any wagering. The scores data layer is untouched: this is a separate
// WebSocket hub that runs alongside the existing routes.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	maxChatHistory = 50   // chat messages kept per room
	maxPickFeed    = 30   // recent picks kept per room
	maxTextLen     = 240  // chars per chat message
	maxNameLen     = 24   // chars per display name
	wsReadLimit    = 8192 // bytes per inbound frame
	sendBuffer     = 32   // queued outbound messages per client
	burstTokens    = 5.0  // rate limit burst
	refillPerSec   = 2.0  // rate limit refill
)

// fanHub holds one room per fixtureId, created on demand.
type fanHub struct {
	mu    sync.Mutex
	rooms map[string]*fanRoom
}

func newFanHub() *fanHub {
	return &fanHub{rooms: map[string]*fanRoom{}}
}

func (h *fanHub) room(id string) *fanRoom {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[id]
	if r == nil {
		r = &fanRoom{
			id:      id,
			clients: map[*fanClient]bool{},
			winner:  map[string]int{"home": 0, "draw": 0, "away": 0},
			corners: map[string]int{"home": 0, "away": 0},
		}
		h.rooms[id] = r
	}
	return r
}

// fanRoom is the live state for one fixture: connected clients, recent chat, the
// prediction tallies, and a short recent-picks feed.
type fanRoom struct {
	id string

	mu      sync.Mutex
	clients map[*fanClient]bool
	chat    []chatEntry
	picks   []pickEntry
	winner  map[string]int // home, draw, away
	corners map[string]int // home, away
}

type chatEntry struct {
	Type string `json:"type"` // always "chat", so it is wire-ready as-is
	Name string `json:"name"`
	Text string `json:"text"`
	Ts   int64  `json:"ts"`
}

type pickEntry struct {
	Name   string `json:"name"`
	Market string `json:"market"` // "winner" | "corners"
	Choice string `json:"choice"` // winner: home/draw/away, corners: home/away
	Ts     int64  `json:"ts"`
}

type tallySnapshot struct {
	Winner  map[string]int `json:"winner"`
	Corners map[string]int `json:"corners"`
}

type initMsg struct {
	Type  string        `json:"type"` // "init"
	Chat  []chatEntry   `json:"chat"`
	Picks []pickEntry   `json:"picks"`
	Tally tallySnapshot `json:"tally"`
}

type pickMsg struct {
	Type  string        `json:"type"` // "pick"
	Entry pickEntry     `json:"entry"`
	Tally tallySnapshot `json:"tally"`
}

// reactionMsg is the ephemeral live reaction broadcast to the room. Reactions are
// not stored: they animate over the pitch and are gone, so they never appear in
// the join snapshot. rid is the sender's nonce echoed back so the originator can
// skip its own reaction (it already animated optimistically on tap).
type reactionMsg struct {
	Type string `json:"type"` // "reaction"
	Icon string `json:"icon"` // fire | clap | shock | sad
	Rid  string `json:"rid"`
	Name string `json:"name"`
}

// inbound is the single shape the client sends. type selects the action; name is
// honoured on any message so a fan who sets a name after connecting is recognised.
type inbound struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Text   string `json:"text"`
	Market string `json:"market"`
	Choice string `json:"choice"`
	Icon   string `json:"icon"`
	Rid    string `json:"rid"`
}

// fanClient is one connected browser. The reader runs in the handler goroutine,
// the writer drains the send channel, and a token bucket caps message rate.
type fanClient struct {
	room *fanRoom
	conn *websocket.Conn
	send chan []byte
	name string

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// handleWS upgrades the request and joins the fixture's room. Query: fixtureId is
// the room key (default "demo"), name is the optional initial display name.
func (h *fanHub) handleWS(w http.ResponseWriter, r *http.Request) {
	fixtureID := strings.TrimSpace(r.URL.Query().Get("fixtureId"))
	if fixtureID == "" {
		fixtureID = "demo"
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(wsReadLimit)

	room := h.room(fixtureID)
	cl := &fanClient{
		room:   room,
		conn:   conn,
		send:   make(chan []byte, sendBuffer),
		name:   sanitizeName(r.URL.Query().Get("name")),
		tokens: burstTokens,
		last:   time.Now(),
	}
	room.join(cl)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer room.leave(cl)
	defer conn.Close(websocket.StatusNormalClosure, "")

	go cl.writeLoop(ctx)
	cl.enqueue(room.snapshot())

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		cl.handleMessage(data)
	}
}

func (c *fanClient) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// enqueue queues a frame for this client, dropping it if the client is too slow
// to drain, so one stalled browser never blocks the room.
func (c *fanClient) enqueue(b []byte) {
	if b == nil {
		return
	}
	select {
	case c.send <- b:
	default:
	}
}

// allow is a simple token bucket: refillPerSec tokens a second, burstTokens max.
func (c *fanClient) allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.tokens += now.Sub(c.last).Seconds() * refillPerSec
	if c.tokens > burstTokens {
		c.tokens = burstTokens
	}
	c.last = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

func (c *fanClient) handleMessage(data []byte) {
	if !c.allow() {
		return
	}
	var in inbound
	if err := json.Unmarshal(data, &in); err != nil {
		return
	}
	if n := sanitizeName(in.Name); n != "" {
		c.name = n
	}
	switch in.Type {
	case "chat":
		text := sanitizeText(in.Text)
		if text == "" {
			return
		}
		c.room.addChat(c.displayName(), text)
	case "pick":
		c.room.addPick(c.displayName(), in.Market, in.Choice)
	case "reaction":
		icon := normalizeReaction(in.Icon)
		if icon == "" {
			return
		}
		c.room.addReaction(c.displayName(), icon, in.Rid)
	}
}

// normalizeReaction accepts only the four known reaction icons, lower-cased.
func normalizeReaction(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fire":
		return "fire"
	case "clap":
		return "clap"
	case "shock":
		return "shock"
	case "sad":
		return "sad"
	}
	return ""
}

func (c *fanClient) displayName() string {
	if c.name == "" {
		return "Someone"
	}
	return c.name
}

func (r *fanRoom) join(c *fanClient) {
	r.mu.Lock()
	r.clients[c] = true
	r.mu.Unlock()
}

func (r *fanRoom) leave(c *fanClient) {
	r.mu.Lock()
	delete(r.clients, c)
	r.mu.Unlock()
}

// broadcast fans a pre-marshalled frame out to every client in the room.
func (r *fanRoom) broadcast(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for c := range r.clients {
		c.enqueue(b)
	}
}

func (r *fanRoom) addChat(name, text string) {
	e := chatEntry{Type: "chat", Name: name, Text: text, Ts: time.Now().UnixMilli()}
	r.mu.Lock()
	r.chat = append(r.chat, e)
	if len(r.chat) > maxChatHistory {
		r.chat = r.chat[len(r.chat)-maxChatHistory:]
	}
	r.mu.Unlock()
	if b, err := json.Marshal(e); err == nil {
		r.broadcast(b)
	}
}

func (r *fanRoom) addPick(name, market, choice string) {
	market = strings.ToLower(strings.TrimSpace(market))
	choice = strings.ToLower(strings.TrimSpace(choice))
	r.mu.Lock()
	ok := false
	switch market {
	case "winner":
		if choice == "home" || choice == "draw" || choice == "away" {
			r.winner[choice]++
			ok = true
		}
	case "corners":
		if choice == "home" || choice == "away" {
			r.corners[choice]++
			ok = true
		}
	}
	if !ok {
		r.mu.Unlock()
		return
	}
	e := pickEntry{Name: name, Market: market, Choice: choice, Ts: time.Now().UnixMilli()}
	r.picks = append(r.picks, e)
	if len(r.picks) > maxPickFeed {
		r.picks = r.picks[len(r.picks)-maxPickFeed:]
	}
	tally := r.tallyLocked()
	r.mu.Unlock()
	if b, err := json.Marshal(pickMsg{Type: "pick", Entry: e, Tally: tally}); err == nil {
		r.broadcast(b)
	}
}

// addReaction fans a live reaction out to everyone in the room, including the
// sender (whose rid lets it dedupe its own optimistic animation). Reactions are
// ephemeral, so nothing is stored.
func (r *fanRoom) addReaction(name, icon, rid string) {
	rid = cleanRunes(rid, 40)
	if b, err := json.Marshal(reactionMsg{Type: "reaction", Icon: icon, Rid: rid, Name: name}); err == nil {
		r.broadcast(b)
	}
}

// snapshot is the init frame a client gets on join: the chat backlog, the recent
// picks, and the current tallies.
func (r *fanRoom) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	msg := initMsg{
		Type:  "init",
		Chat:  append([]chatEntry(nil), r.chat...),
		Picks: append([]pickEntry(nil), r.picks...),
		Tally: r.tallyLocked(),
	}
	b, _ := json.Marshal(msg)
	return b
}

// tallyLocked returns a fresh copy of the counts. Caller holds r.mu.
func (r *fanRoom) tallyLocked() tallySnapshot {
	return tallySnapshot{
		Winner:  map[string]int{"home": r.winner["home"], "draw": r.winner["draw"], "away": r.winner["away"]},
		Corners: map[string]int{"home": r.corners["home"], "away": r.corners["away"]},
	}
}

func sanitizeText(s string) string { return cleanRunes(s, maxTextLen) }
func sanitizeName(s string) string { return cleanRunes(s, maxNameLen) }

// cleanRunes trims, drops control characters (turning newlines and tabs into
// spaces), and caps the length in runes so multibyte names are not cut mid-rune.
func cleanRunes(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == '\r' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
