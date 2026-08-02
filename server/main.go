package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed static
var staticFiles embed.FS

// Heartbeat tracks when MikroTik last fetched state
type Heartbeat struct {
	mu            sync.RWMutex
	lastSeen      time.Time
	routerVersion string   // script version reported by router
	serverVersion string   // script version extracted from .rsc on server
	seenParams    []string // hook tags the router found last cycle (X-Seen-Params)
}

func (h *Heartbeat) Touch(routerVersion string, seenParams []string) {
	h.mu.Lock()
	h.lastSeen = time.Now()
	h.routerVersion = routerVersion
	if seenParams != nil {
		h.seenParams = seenParams
	}
	h.mu.Unlock()
}

type heartbeatResponse struct {
	LastSeen       *int64   `json:"last_seen"`                 // unix timestamp, nil = never
	AgeSec         int      `json:"age_sec"`                   // seconds since last seen
	ScriptOutdated bool     `json:"script_outdated,omitempty"` // true if router script != server script
	SeenParams     []string `json:"seen_params,omitempty"`     // hook tags present on the router
}

func (h *Heartbeat) Info() heartbeatResponse {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.lastSeen.IsZero() {
		return heartbeatResponse{}
	}
	ts := h.lastSeen.Unix()
	outdated := h.serverVersion != "" && h.routerVersion != h.serverVersion
	seen := make([]string, len(h.seenParams))
	copy(seen, h.seenParams)
	return heartbeatResponse{
		LastSeen:       &ts,
		AgeSec:         int(time.Since(h.lastSeen).Seconds()),
		ScriptOutdated: outdated,
		SeenParams:     seen,
	}
}

func (h *Heartbeat) IsScriptOutdated() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.serverVersion != "" && h.routerVersion != h.serverVersion
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

	audit := NewAuditLog(filepath.Join(dataDir, "audit.json"), 2000)
	hb := &Heartbeat{serverVersion: extractScriptVersion("/mikrotik/remote-hook.rsc")}

	// Background ticker: revert params whose timer has expired
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for range ticker.C {
			for _, name := range store.RestoreExpired() {
				audit.Add(name, "expired", "таймер истёк")
			}
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		handleGetState(w, r, store, hb)
	})
	mux.HandleFunc("POST /api/state", func(w http.ResponseWriter, r *http.Request) {
		handleSetState(w, r, store, audit)
	})
	mux.HandleFunc("POST /api/group", func(w http.ResponseWriter, r *http.Request) {
		handleSetGroup(w, r, store, audit)
	})
	mux.HandleFunc("POST /api/timer", func(w http.ResponseWriter, r *http.Request) {
		handleTimer(w, r, store, audit)
	})
	mux.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		handlePause(w, r, store, audit)
	})
	mux.HandleFunc("POST /api/resume", func(w http.ResponseWriter, r *http.Request) {
		handleResume(w, r, store, audit)
	})
	mux.HandleFunc("POST /api/preset", func(w http.ResponseWriter, r *http.Request) {
		handleApplyPreset(w, r, store, audit)
	})
	mux.HandleFunc("POST /api/params", func(w http.ResponseWriter, r *http.Request) {
		handleAddParam(w, r, store, audit)
	})
	mux.HandleFunc("DELETE /api/params", func(w http.ResponseWriter, r *http.Request) {
		handleDeleteParam(w, r, store, audit)
	})
	mux.HandleFunc("POST /api/groups", func(w http.ResponseWriter, r *http.Request) {
		handleAddGroup(w, r, store)
	})
	mux.HandleFunc("DELETE /api/groups", func(w http.ResponseWriter, r *http.Request) {
		handleDeleteGroup(w, r, store)
	})

	mux.HandleFunc("GET /api/templates", func(w http.ResponseWriter, r *http.Request) {
		handleGetTemplates(w, r, store)
	})
	mux.HandleFunc("POST /api/templates/import", func(w http.ResponseWriter, r *http.Request) {
		handleImportTemplates(w, r, store, audit)
	})
	mux.HandleFunc("GET /mikrotik/templates.rsc", func(w http.ResponseWriter, r *http.Request) {
		handleTemplatesRsc(w, r, store)
	})

	mux.HandleFunc("GET /api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hb.Info())
	})

	mux.HandleFunc("GET /api/log", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(audit.Recent(50))
	})

	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		daysStr := r.URL.Query().Get("days")
		if daysStr != "" {
			days := 7
			fmt.Sscanf(daysStr, "%d", &days)
			if days < 1 {
				days = 1
			}
			if days > 90 {
				days = 90
			}
			json.NewEncoder(w).Encode(audit.StatsFiltered(days))
		} else {
			json.NewEncoder(w).Encode(audit.Stats())
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

	srv := &http.Server{Addr: listenAddr, Handler: handler}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		audit.Flush()
		srv.Shutdown(context.Background())
	}()

	log.Printf("listening on %s", listenAddr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// extractScriptVersion reads the .rsc file and extracts scriptVersion value.
func extractScriptVersion(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Printf("WARN: cannot read script %s: %v", path, err)
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		// :local scriptVersion "1"
		if strings.HasPrefix(line, ":local scriptVersion") {
			q1 := strings.Index(line, "\"")
			q2 := strings.LastIndex(line, "\"")
			if q1 != -1 && q2 > q1 {
				return line[q1+1 : q2]
			}
		}
	}
	return ""
}
