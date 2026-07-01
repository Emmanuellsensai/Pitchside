// Fixtures listing for the Matches screen.
//
// Fetches the same upstream snapshot the replay relabel and push watcher already
// use, then normalises each fixture to a small, stable shape the frontend can
// render: id, competition, home, away, kickoff (epoch ms), status, and a score
// when the snapshot carries one. The scores data layer is untouched: this is a
// read-only view over the snapshot the proxy already proxies.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// fixtureOut is one normalised fixture row.
type fixtureOut struct {
	ID          string `json:"id"`
	Competition string `json:"competition"`
	Home        string `json:"home"`
	Away        string `json:"away"`
	Kickoff     int64  `json:"kickoff"` // epoch ms, 0 if unknown
	Status      string `json:"status"`  // live | upcoming | finished
	HomeScore   *int   `json:"homeScore,omitempty"`
	AwayScore   *int   `json:"awayScore,omitempty"`
}

// liveWindowMs is how long after kickoff a match is treated as live when the
// snapshot gives no explicit status (regulation, stoppage, and a margin).
const liveWindowMs = int64(150 * 60 * 1000)

// handleFixtures returns the normalised fixtures list as a JSON array.
func (c Config) handleFixtures(w http.ResponseWriter, r *http.Request) {
	raw, err := c.fetchSnapshot(r)
	if err != nil {
		http.Error(w, "upstream snapshot failed", http.StatusBadGateway)
		return
	}

	list := decodeFixtureList(raw)
	now := time.Now().UnixMilli()
	out := make([]fixtureOut, 0, len(list))
	for _, m := range list {
		f := normalizeFixture(m, now)
		if f.ID == "" {
			continue
		}
		out = append(out, f)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// fetchSnapshot pulls the upstream fixtures snapshot with the token attached.
func (c Config) fetchSnapshot(r *http.Request) ([]byte, error) {
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, c.Base+"/api/fixtures/snapshot", nil)
	if err != nil {
		return nil, err
	}
	upReq.Header.Set("Authorization", "Bearer "+c.JWT)
	upReq.Header.Set("X-Api-Token", c.APIToken)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(upReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errStatus(resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// decodeFixtureList accepts either a bare array or a wrapper object holding the
// array under fixtures/data/snapshot, so it survives small upstream shape changes.
func decodeFixtureList(raw []byte) []map[string]any {
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var wrap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrap); err == nil {
		for _, key := range []string{"fixtures", "data", "snapshot", "Fixtures", "Data"} {
			if v, ok := wrap[key]; ok {
				if err := json.Unmarshal(v, &arr); err == nil {
					return arr
				}
			}
		}
	}
	return nil
}

// normalizeFixture maps one upstream record onto fixtureOut, deriving the status
// from an explicit field when present and otherwise from the kickoff time.
func normalizeFixture(m map[string]any, now int64) fixtureOut {
	p1 := getString(m, "Participant1", "participant1", "HomeTeam", "Home", "home")
	p2 := getString(m, "Participant2", "participant2", "AwayTeam", "Away", "away")
	p1Home, hasP1Home := getBool(m, "Participant1IsHome", "participant1_is_home")
	home, away := p1, p2
	if hasP1Home && !p1Home {
		home, away = p2, p1
	}

	f := fixtureOut{
		ID:          getString(m, "FixtureId", "fixture_id", "fixtureId", "id", "Id"),
		Competition: getString(m, "Competition", "competition", "CompetitionName", "League", "league"),
		Home:        home,
		Away:        away,
		Kickoff:     getKickoff(m),
	}

	// Scores, mapped through the same participant-to-side orientation as the names.
	s1, ok1 := getInt(m, "Participant1Score", "Score1", "HomeScore", "homeScore")
	s2, ok2 := getInt(m, "Participant2Score", "Score2", "AwayScore", "awayScore")
	if ok1 && ok2 {
		hs, as := s1, s2
		if hasP1Home && !p1Home {
			hs, as = s2, s1
		}
		f.HomeScore = &hs
		f.AwayScore = &as
	}

	f.Status = deriveStatus(m, f.Kickoff, now)
	return f
}

// deriveStatus prefers an explicit status or game-state field and falls back to a
// kickoff-time window when none is given.
func deriveStatus(m map[string]any, kickoff, now int64) string {
	if s := explicitStatus(m); s != "" {
		return s
	}
	if kickoff <= 0 {
		return "upcoming"
	}
	if now < kickoff {
		return "upcoming"
	}
	if now < kickoff+liveWindowMs {
		return "live"
	}
	return "finished"
}

// explicitStatus reads a status or game-state field if the snapshot carries one,
// returning "" when there is nothing usable so the caller falls back to time.
func explicitStatus(m map[string]any) string {
	if gs, ok := getInt(m, "GameState", "gameState", "Phase", "phase"); ok {
		switch gs {
		case 1:
			return "upcoming"
		case 2, 3, 4, 6, 7:
			return "live"
		case 5:
			return "finished"
		}
	}
	s := strings.ToLower(getString(m, "Status", "status", "State", "state", "GameState", "gameState"))
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r
		}
		return -1
	}, s)
	if s == "" {
		return ""
	}
	switch {
	case strings.Contains(s, "live"), strings.Contains(s, "inplay"), strings.Contains(s, "playing"),
		strings.Contains(s, "firsthalf"), strings.Contains(s, "secondhalf"), strings.Contains(s, "halftime"):
		return "live"
	case strings.Contains(s, "finish"), strings.Contains(s, "ended"), strings.Contains(s, "fulltime"),
		strings.Contains(s, "complete"), s == "ft" || s == "final":
		return "finished"
	case strings.Contains(s, "sched"), strings.Contains(s, "notstarted"), strings.Contains(s, "upcoming"),
		strings.Contains(s, "prematch"), strings.Contains(s, "pregame"):
		return "upcoming"
	}
	return ""
}

// getKickoff reads the kickoff timestamp and normalises it to epoch milliseconds,
// accepting epoch seconds, epoch millis, or an RFC3339 date string.
func getKickoff(m map[string]any) int64 {
	for _, k := range []string{"StartTime", "startTime", "KickoffTime", "Kickoff", "kickoff", "StartDate", "startDate", "Date"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			ms := int64(n)
			if ms < 1e11 { // looks like epoch seconds
				ms *= 1000
			}
			return ms
		case json.Number:
			if i, err := n.Int64(); err == nil {
				if i < 1e11 {
					i *= 1000
				}
				return i
			}
		case string:
			if t, err := time.Parse(time.RFC3339, n); err == nil {
				return t.UnixMilli()
			}
		}
	}
	return 0
}

// getString returns the first non-empty string value among the given keys.
func getString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// getBool returns the first boolean value among the given keys, and whether one
// was found.
func getBool(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if b, ok := v.(bool); ok {
				return b, true
			}
		}
	}
	return false, false
}

// getInt returns the first integer value among the given keys, and whether one
// was found. JSON numbers decode as float64, so they are truncated here.
func getInt(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return int(n), true
			case json.Number:
				if i, err := n.Int64(); err == nil {
					return int(i), true
				}
			case string:
				if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
					return i, true
				}
			}
		}
	}
	return 0, false
}
