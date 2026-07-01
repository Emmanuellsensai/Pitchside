// Web push for Pitchside.
//
// Holds browser push subscriptions in memory, exposes the VAPID public key and a
// subscribe endpoint, and runs a background watcher that reads the same upstream
// scores stream the browser sees and sends a web-push notification on each goal,
// corner, card, and bottle event. The stat decoding mirrors the frontend parser:
//
//	stat key = (period * 1000) + base, base = key % 1000
//	base 1/2 goals, 3/4 yellow, 5/6 red, 7/8 corners (home, away)
//
// Everything stays devnet and free. The TxLINE token is never logged.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// PushSub is one stored browser subscription plus the context the watcher needs
// to target bottle alerts (the favourite side and the fixture the fan is on).
type PushSub struct {
	Subscription webpush.Subscription `json:"subscription"`
	Fav          string               `json:"fav"`       // "home", "away", or ""
	FixtureID    string               `json:"fixtureId"` // "" means any fixture
	Teams        struct {
		Home string `json:"home"`
		Away string `json:"away"`
	} `json:"teams"`
}

// teamNames holds the home and away labels for one fixture.
type teamNames struct{ Home, Away string }

// fixState is the running per-fixture tally the watcher compares against to spot
// the next goal, card, or corner, and the lead state used for the bottle watch.
type fixState struct {
	seeded          bool
	goalsH, goalsA  int
	corH, corA      int
	yelH, yelA      int
	redH, redA      int
	leadH, bottledH bool
	leadA, bottledA bool
}

// PushHub stores subscriptions and the VAPID material, and tracks per-fixture
// state plus the resolved team names.
type PushHub struct {
	mu        sync.Mutex
	subs      map[string]PushSub   // keyed by endpoint
	state     map[string]*fixState // keyed by fixture id
	teams     map[string]teamNames // keyed by fixture id
	vapidPub  string
	vapidPriv string
	subject   string
}

func newPushHub() *PushHub {
	pub := strings.TrimSpace(envOr("VAPID_PUBLIC_KEY", ""))
	priv := strings.TrimSpace(envOr("VAPID_PRIVATE_KEY", ""))
	if pub == "" || priv == "" {
		gp, gpub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			log.Fatalf("could not generate VAPID keys: %v", err)
		}
		priv, pub = gp, gpub
		log.Printf("No VAPID keys in env, generated an ephemeral pair for this run.")
		log.Printf("Set these in the proxy env to keep subscriptions working across restarts:")
		log.Printf("  VAPID_PUBLIC_KEY=%s", pub)
		log.Printf("  VAPID_PRIVATE_KEY=%s", priv)
	}
	return &PushHub{
		subs:      map[string]PushSub{},
		state:     map[string]*fixState{},
		teams:     map[string]teamNames{},
		vapidPub:  pub,
		vapidPriv: priv,
		subject:   envOr("VAPID_SUBJECT", "mailto:pitchside@example.com"),
	}
}

// handleVapidKey returns the public key the browser needs to subscribe.
func (h *PushHub) handleVapidKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(h.vapidPub))
}

// handleSubscribe stores a browser subscription in memory. A repeat post for the
// same endpoint updates the favourite and fixture so the bottle watch stays current.
func (h *PushHub) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var sub PushSub
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.Subscription.Endpoint == "" {
		http.Error(w, "invalid subscription", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	h.subs[sub.Subscription.Endpoint] = sub
	if sub.FixtureID != "" && (sub.Teams.Home != "" || sub.Teams.Away != "") {
		h.teams[sub.FixtureID] = teamNames{Home: sub.Teams.Home, Away: sub.Teams.Away}
	}
	n := len(h.subs)
	h.mu.Unlock()
	log.Printf("push: stored subscription (%d total)", n)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(`{"ok":true}`))
}

type pushPayload struct {
	Title     string `json:"title"`
	Body      string `json:"body"`
	Tag       string `json:"tag"`
	Type      string `json:"type"`
	FixtureID string `json:"fixtureId"`
}

// send delivers one notification to every subscription that passes the filter,
// dropping subscriptions the push service reports as gone.
func (h *PushHub) send(p pushPayload, keep func(PushSub) bool) {
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	h.mu.Lock()
	targets := make([]PushSub, 0, len(h.subs))
	for _, s := range h.subs {
		if keep == nil || keep(s) {
			targets = append(targets, s)
		}
	}
	h.mu.Unlock()

	var dead []string
	for _, s := range targets {
		sub := s.Subscription
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := webpush.SendNotificationWithContext(ctx, body, &sub, &webpush.Options{
			Subscriber:      h.subject,
			VAPIDPublicKey:  h.vapidPub,
			VAPIDPrivateKey: h.vapidPriv,
			TTL:             60,
		})
		cancel()
		if err != nil {
			continue
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code == http.StatusNotFound || code == http.StatusGone {
			dead = append(dead, sub.Endpoint)
		}
	}
	if len(dead) > 0 {
		h.mu.Lock()
		for _, e := range dead {
			delete(h.subs, e)
		}
		h.mu.Unlock()
	}
}

func (h *PushHub) namesFor(fixtureID string) teamNames {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.teams[fixtureID]; ok {
		return t
	}
	return teamNames{Home: "Home", Away: "Away"}
}

func teamLabel(side string, t teamNames) string {
	if side == "home" {
		if t.Home != "" {
			return t.Home
		}
		return "Home"
	}
	if t.Away != "" {
		return t.Away
	}
	return "Away"
}

// runWatcher keeps a connection to the upstream scores stream and reconnects on
// any drop, so push keeps flowing for as long as the proxy is up.
func (h *PushHub) runWatcher(cfg Config) {
	h.refreshTeams(cfg)
	go func() {
		for range time.Tick(5 * time.Minute) {
			h.refreshTeams(cfg)
		}
	}()
	for {
		if err := h.watchOnce(cfg); err != nil {
			log.Printf("push watcher: stream ended (%v), reconnecting", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func (h *PushHub) watchOnce(cfg Config) error {
	req, err := http.NewRequest(http.MethodGet, cfg.Base+"/api/scores/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.JWT)
	req.Header.Set("X-Api-Token", cfg.APIToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errStatus(resp.StatusCode)
	}
	log.Printf("push watcher: connected to upstream scores stream")

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		h.handleUpdate([]byte(payload))
	}
	return sc.Err()
}

type scoreUpdate struct {
	FixtureID json.Number            `json:"FixtureId"`
	Ts        int64                  `json:"Ts"`
	StartTime int64                  `json:"StartTime"`
	Stats     map[string]interface{} `json:"Stats"`
}

// handleUpdate decodes one score update, advances the per-fixture tallies, and
// emits a notification for every new goal, card, corner, and bottle.
func (h *PushHub) handleUpdate(raw []byte) {
	var u scoreUpdate
	if err := json.Unmarshal(raw, &u); err != nil {
		return
	}
	fixtureID := u.FixtureID.String()
	if fixtureID == "" || len(u.Stats) == 0 {
		return
	}

	// Collapse the per-period counters onto the period 0 totals, exactly like the
	// frontend, by taking the running max per base.
	var gH, gA, cH, cA, yH, yA, rH, rA int
	for k, v := range u.Stats {
		n := toInt(v)
		switch baseKey(k) {
		case 1:
			gH = max(gH, n)
		case 2:
			gA = max(gA, n)
		case 3:
			yH = max(yH, n)
		case 4:
			yA = max(yA, n)
		case 5:
			rH = max(rH, n)
		case 6:
			rA = max(rA, n)
		case 7:
			cH = max(cH, n)
		case 8:
			cA = max(cA, n)
		}
	}

	h.mu.Lock()
	st := h.state[fixtureID]
	if st == nil {
		// First sight of this fixture: seed the baseline without alerting so a
		// mid-match connect or a reconnect does not replay the whole match.
		st = &fixState{
			seeded: true,
			goalsH: gH, goalsA: gA, corH: cH, corA: cA,
			yelH: yH, yelA: yA, redH: rH, redA: rA,
		}
		st.leadH = gH-gA >= 1
		st.leadA = gA-gH >= 1
		h.state[fixtureID] = st
		h.mu.Unlock()
		return
	}

	type ev struct {
		typ, side string
	}
	var events []ev
	if gH > st.goalsH {
		st.goalsH = gH
		events = append(events, ev{"goal", "home"})
	}
	if gA > st.goalsA {
		st.goalsA = gA
		events = append(events, ev{"goal", "away"})
	}
	if cH > st.corH {
		st.corH = cH
		events = append(events, ev{"corner", "home"})
	}
	if cA > st.corA {
		st.corA = cA
		events = append(events, ev{"corner", "away"})
	}
	if yH > st.yelH {
		st.yelH = yH
		events = append(events, ev{"yellow", "home"})
	}
	if yA > st.yelA {
		st.yelA = yA
		events = append(events, ev{"yellow", "away"})
	}
	if rH > st.redH {
		st.redH = rH
		events = append(events, ev{"red", "home"})
	}
	if rA > st.redA {
		st.redA = rA
		events = append(events, ev{"red", "away"})
	}

	// Bottle watch: a side that had a lead and is now level or behind. Evaluate it
	// here so it shares the same locked state as the score tally.
	var bottles []string
	if len(events) > 0 {
		dH := st.goalsH - st.goalsA
		if dH >= 1 {
			st.leadH, st.bottledH = true, false
		} else if st.leadH && dH <= 0 && !st.bottledH {
			st.bottledH = true
			bottles = append(bottles, "home")
		}
		dA := st.goalsA - st.goalsH
		if dA >= 1 {
			st.leadA, st.bottledA = true, false
		} else if st.leadA && dA <= 0 && !st.bottledA {
			st.bottledA = true
			bottles = append(bottles, "away")
		}
	}
	gh, ga := st.goalsH, st.goalsA
	h.mu.Unlock()

	if len(events) == 0 && len(bottles) == 0 {
		return
	}

	names := h.namesFor(fixtureID)
	minute := deriveMinute(u.StartTime, u.Ts)
	score := names.Home + " " + strconv.Itoa(gh) + " - " + strconv.Itoa(ga) + " " + names.Away

	atFix := func(s PushSub) bool { return s.FixtureID == "" || s.FixtureID == fixtureID }

	for _, e := range events {
		team := teamLabel(e.side, names)
		var p pushPayload
		p.Type, p.FixtureID = e.typ, fixtureID
		switch e.typ {
		case "goal":
			p.Title = "Goal, " + team
			p.Body = score + minuteSuffix(minute)
			p.Tag = "goal-" + fixtureID
		case "corner":
			p.Title = "Corner, " + team
			p.Body = score + minuteSuffix(minute)
			p.Tag = "corner-" + fixtureID
		case "yellow":
			p.Title = "Yellow card, " + team
			p.Body = score + minuteSuffix(minute)
			p.Tag = "card-" + fixtureID
		case "red":
			p.Title = "Red card, " + team
			p.Body = score + minuteSuffix(minute)
			p.Tag = "card-" + fixtureID
		}
		h.send(p, atFix)
	}

	for _, side := range bottles {
		team := teamLabel(side, names)
		p := pushPayload{
			Title:     "Bottle alert, " + team,
			Body:      team + " have let the lead slip. " + score + minuteSuffix(minute),
			Tag:       "bottle-" + fixtureID,
			Type:      "bottle",
			FixtureID: fixtureID,
		}
		h.send(p, func(s PushSub) bool { return atFix(s) && s.Fav == side })
	}
}

// refreshTeams resolves fixture team names from the snapshot so notifications can
// name the side. Best effort: failures leave whatever names are already known.
func (h *PushHub) refreshTeams(cfg Config) {
	req, err := http.NewRequest(http.MethodGet, cfg.Base+"/api/fixtures/snapshot", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.JWT)
	req.Header.Set("X-Api-Token", cfg.APIToken)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var list []struct {
		FixtureID          json.Number `json:"FixtureId"`
		Participant1       string      `json:"Participant1"`
		Participant2       string      `json:"Participant2"`
		Participant1IsHome bool        `json:"Participant1IsHome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return
	}
	h.mu.Lock()
	for _, f := range list {
		id := f.FixtureID.String()
		if id == "" {
			continue
		}
		home, away := f.Participant1, f.Participant2
		if !f.Participant1IsHome {
			home, away = f.Participant2, f.Participant1
		}
		h.teams[id] = teamNames{Home: home, Away: away}
	}
	h.mu.Unlock()
}

// baseKey strips the period multiplier from a stat key, returning the base stat.
func baseKey(k string) int {
	n, err := strconv.Atoi(strings.TrimSpace(k))
	if err != nil {
		return -1
	}
	return n % 1000
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

// deriveMinute is a rough match minute from the kickoff timestamp, clamped to a
// sane range. The notification only needs an approximate time.
func deriveMinute(startTime, ts int64) int {
	if startTime <= 0 || ts <= 0 {
		return -1
	}
	m := int((ts - startTime) / 60000)
	if m < 0 {
		return 0
	}
	if m > 120 {
		return 120
	}
	return m
}

func minuteSuffix(m int) string {
	if m < 0 {
		return ""
	}
	return " (" + strconv.Itoa(m) + "')"
}

type statusError int

func (e statusError) Error() string { return "upstream status " + strconv.Itoa(int(e)) }
func errStatus(code int) error      { return statusError(code) }
