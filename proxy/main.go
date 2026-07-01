// Touchline token proxy.
//
// Holds the TxLINE JWT and API token, opens the upstream SSE stream, and
// re-streams it to the browser so the token never reaches the client. Also
// proxies the historical endpoint and serves the static frontend.
//
// Standard library only. The token is never logged.
package main

import (
	"bufio"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is read from the environment (proxy/.env is loaded first if present).
type Config struct {
	JWT      string
	APIToken string
	Base     string
	Port     string
	WebDir   string
}

func loadConfig() Config {
	loadDotEnv(".env")
	return Config{
		JWT:      os.Getenv("TXLINE_JWT"),
		APIToken: os.Getenv("TXLINE_API_TOKEN"),
		Base:     envOr("TXLINE_BASE", "https://txline-dev.txodds.com"),
		Port:     envOr("PORT", "8080"),
		WebDir:   envOr("WEB_DIR", "../web"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadDotEnv reads a simple KEY=VALUE file and sets any keys not already present
// in the environment, so an exported variable always wins over the file. Lines
// that are blank or start with # are ignored. The value is taken verbatim after
// the first '=' (surrounding quotes are stripped). Missing file is not an error.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, ok := os.LookupEnv(key); !ok {
			os.Setenv(key, val)
		}
	}
}

func main() {
	cfg := loadConfig()
	if cfg.JWT == "" || cfg.APIToken == "" {
		log.Fatal("TXLINE_JWT and TXLINE_API_TOKEN must be set (see proxy/.env.example)")
	}

	// Web push: in-memory subscriptions plus a background watcher that turns the
	// upstream scores stream into goal, corner, card, and bottle notifications.
	hub := newPushHub()
	go hub.runWatcher(cfg)

	// Fan Zone: live chat and a fan-sentiment prediction board, one room per
	// fixtureId. In-memory only, separate from the scores data layer.
	fan := newFanHub()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/stream/scores", cfg.handleStream)
	mux.HandleFunc("GET /api/scores/historical/{fixtureId}", cfg.handleHistorical)
	mux.HandleFunc("GET /api/fixtures/snapshot", cfg.handleSnapshot)
	mux.HandleFunc("GET /api/push/vapidPublicKey", hub.handleVapidKey)
	mux.HandleFunc("POST /api/push/subscribe", hub.handleSubscribe)
	mux.HandleFunc("GET /ws", fan.handleWS)
	mux.Handle("/", cfg.staticHandler())

	addr := ":" + cfg.Port
	log.Printf("Touchline proxy listening on %s, upstream %s, serving %s", addr, cfg.Base, cfg.WebDir)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// No write timeout: the SSE relay is a long-lived response.
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// handleStream opens the upstream SSE stream and relays it to the client
// unbuffered, flushing after every write. The token is attached server side.
func (c Config) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Tie the upstream request to the client connection so an upstream that
	// never ends is cancelled the moment the browser disconnects.
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, c.Base+"/api/scores/stream", nil)
	if err != nil {
		http.Error(w, "bad upstream request", http.StatusInternalServerError)
		return
	}
	upReq.Header.Set("Authorization", "Bearer "+c.JWT)
	upReq.Header.Set("X-Api-Token", c.APIToken)
	upReq.Header.Set("Accept", "text/event-stream")
	upReq.Header.Set("Cache-Control", "no-cache")

	// No client timeout: this response streams indefinitely.
	resp, err := (&http.Client{}).Do(upReq)
	if err != nil {
		http.Error(w, "upstream connect failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Surface the upstream status without leaking credentials.
		http.Error(w, "upstream stream status "+resp.Status, http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return // client gone
			}
			flusher.Flush()
		}
		if readErr != nil {
			return // io.EOF or upstream/context error ends the relay
		}
	}
}

// handleHistorical proxies a GET for one fixture's history and returns the JSON
// as-is, attaching the token server side.
func (c Config) handleHistorical(w http.ResponseWriter, r *http.Request) {
	fixtureID := r.PathValue("fixtureId")
	if fixtureID == "" {
		http.Error(w, "missing fixtureId", http.StatusBadRequest)
		return
	}
	c.proxyGet(w, r, "/api/scores/historical/"+fixtureID)
}

// handleSnapshot proxies the fixtures snapshot, the list of bundled fixtures with
// team names and kickoff times. The browser uses it to label a replay with the
// real teams. Returns the JSON as-is, attaching the token server side.
func (c Config) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	c.proxyGet(w, r, "/api/fixtures/snapshot")
}

// proxyGet performs a GET against the upstream path with the token headers and
// returns the JSON body verbatim. Shared by the historical and snapshot routes.
func (c Config) proxyGet(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, c.Base+upstreamPath, nil)
	if err != nil {
		http.Error(w, "bad upstream request", http.StatusInternalServerError)
		return
	}
	upReq.Header.Set("Authorization", "Bearer "+c.JWT)
	upReq.Header.Set("X-Api-Token", c.APIToken)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(upReq)
	if err != nil {
		http.Error(w, "upstream connect failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// staticHandler serves the web directory. The root path serves index.html if it
// exists, otherwise touchline.html, so the single-page frontend loads at /.
func (c Config) staticHandler() http.Handler {
	fs := http.FileServer(http.Dir(c.WebDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			index := filepath.Join(c.WebDir, "index.html")
			if _, err := os.Stat(index); err == nil {
				http.ServeFile(w, r, index)
				return
			}
			http.ServeFile(w, r, filepath.Join(c.WebDir, "touchline.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})
}
