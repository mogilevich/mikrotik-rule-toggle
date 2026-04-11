package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFiles embed.FS

// Heartbeat tracks when MikroTik last fetched state
type Heartbeat struct {
	mu       sync.RWMutex
	lastSeen time.Time
}

func (h *Heartbeat) Touch() {
	h.mu.Lock()
	h.lastSeen = time.Now()
	h.mu.Unlock()
}

type heartbeatResponse struct {
	LastSeen *int64 `json:"last_seen"` // unix timestamp, nil = never
	AgeSec   int    `json:"age_sec"`   // seconds since last seen
}

func (h *Heartbeat) Info() heartbeatResponse {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.lastSeen.IsZero() {
		return heartbeatResponse{}
	}
	ts := h.lastSeen.Unix()
	return heartbeatResponse{
		LastSeen: &ts,
		AgeSec:   int(time.Since(h.lastSeen).Seconds()),
	}
}

func main() {
	listenAddr := envOrDefault("LISTEN_ADDR", ":8080")
	dataDir := envOrDefault("DATA_DIR", "./data")
	authToken := os.Getenv("AUTH_TOKEN")

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatal(err)
	}

	store, err := NewStore(filepath.Join(dataDir, "state.json"))
	if err != nil {
		log.Fatal(err)
	}

	hb := &Heartbeat{}

	// Background ticker: re-enable params whose timer has expired
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for range ticker.C {
			store.RestoreExpired()
		}
	}()

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleGetState(w, r, store, hb)
		case http.MethodPost:
			handleSetState(w, r, store)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hb.Info())
	})

	mux.HandleFunc("/api/timer", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleTimer(w, r, store)
	})

	mux.HandleFunc("/api/params", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			handleAddParam(w, r, store)
		case http.MethodDelete:
			handleDeleteParam(w, r, store)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Serve MikroTik script for download
	mux.HandleFunc("/mikrotik/remote-hook.rsc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.ServeFile(w, r, "/mikrotik/remote-hook.rsc")
	})

	// Static files
	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	handler := withAuth(authToken, mux)

	log.Printf("listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, handler))
}

func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func handleGetState(w http.ResponseWriter, r *http.Request, store *Store, hb *Heartbeat) {
	// Detect MikroTik fetch by User-Agent
	ua := r.Header.Get("User-Agent")
	if strings.Contains(strings.ToLower(ua), "mikrotik") || strings.Contains(strings.ToLower(ua), "routeros") {
		hb.Touch()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(store.GetState())
}

type setStateReq struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

func handleSetState(w http.ResponseWriter, r *http.Request, store *Store) {
	var req setStateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if !store.SetParam(req.Name, req.Enabled) {
		http.Error(w, "param not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(store.GetState())
}

type timerReq struct {
	Name    string `json:"name"`
	Minutes int    `json:"minutes"`
}

func handleTimer(w http.ResponseWriter, r *http.Request, store *Store) {
	var req timerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Minutes <= 0 {
		http.Error(w, "name and minutes (>0) are required", http.StatusBadRequest)
		return
	}
	if !store.TempRelease(req.Name, time.Duration(req.Minutes)*time.Minute) {
		http.Error(w, "param not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(store.GetState())
}

type addParamReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Inverted    bool   `json:"inverted"`
}

func handleAddParam(w http.ResponseWriter, r *http.Request, store *Store) {
	var req addParamReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	store.AddParam(req.Name, req.Description, req.Inverted)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(store.GetState())
}

func handleDeleteParam(w http.ResponseWriter, r *http.Request, store *Store) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name query param is required", http.StatusBadRequest)
		return
	}
	if !store.DeleteParam(name) {
		http.Error(w, "param not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(store.GetState())
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
