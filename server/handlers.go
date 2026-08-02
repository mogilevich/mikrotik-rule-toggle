package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

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

func writeState(w http.ResponseWriter, store *Store) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(store.GetState())
}

func handleGetState(w http.ResponseWriter, r *http.Request, store *Store, hb *Heartbeat) {
	// Detect MikroTik fetch by User-Agent
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	isMikroTik := strings.Contains(ua, "mikrotik") || strings.Contains(ua, "routeros")
	if !isMikroTik {
		writeState(w, store)
		return
	}
	var seen []string
	if h := r.Header.Get("X-Seen-Params"); h != "" {
		seen = strings.Split(h, ",")
	}
	hb.Touch(r.Header.Get("X-Script-Version"), seen)
	store.ActivatePendingTimers()
	// Router view: params only — the .rsc JSON parser searches substrings,
	// extra top-level keys (groups/presets) must not reach it. params go
	// first so param-name lookups never land on the fields below.
	w.Header().Set("Content-Type", "application/json")
	params := store.GetState().Params
	resp := struct {
		Params        map[string]Param `json:"params"`
		TemplatesHash string           `json:"templates_hash,omitempty"`
		ScriptUpdate  bool             `json:"script_update,omitempty"`
	}{params, templatesHash(importedTemplates(params)), hb.IsScriptOutdated()}
	json.NewEncoder(w).Encode(resp)
}

type setStateReq struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

func handleSetState(w http.ResponseWriter, r *http.Request, store *Store, audit *AuditLog) {
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
	if req.Enabled {
		audit.Add(req.Name, "toggle", "включён")
	} else {
		audit.Add(req.Name, "toggle", "выключен")
	}
	writeState(w, store)
}

type groupReq struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

func handleSetGroup(w http.ResponseWriter, r *http.Request, store *Store, audit *AuditLog) {
	var req groupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if store.SetGroup(req.Name, req.Enabled) == 0 {
		http.Error(w, "group is empty or not found", http.StatusNotFound)
		return
	}
	if req.Enabled {
		audit.Add("группа "+req.Name, "toggle", "включена")
	} else {
		audit.Add("группа "+req.Name, "toggle", "выключена")
	}
	writeState(w, store)
}

type timerReq struct {
	Name    string `json:"name,omitempty"`
	Group   string `json:"group,omitempty"`
	Minutes int    `json:"minutes"`
}

func handleTimer(w http.ResponseWriter, r *http.Request, store *Store, audit *AuditLog) {
	var req timerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if (req.Name == "") == (req.Group == "") || req.Minutes <= 0 {
		http.Error(w, "either name or group, and minutes (>0) are required", http.StatusBadRequest)
		return
	}
	dur := time.Duration(req.Minutes) * time.Minute
	if req.Group != "" {
		if store.TempReleaseGroup(req.Group, dur) == 0 {
			http.Error(w, "group is empty or not found", http.StatusNotFound)
			return
		}
		audit.Add("группа "+req.Group, "timer", formatDuration(req.Minutes))
	} else {
		found, extended := store.TempRelease(req.Name, dur)
		if !found {
			http.Error(w, "param not found", http.StatusNotFound)
			return
		}
		if extended {
			audit.Add(req.Name, "timer", "+"+formatDuration(req.Minutes))
		} else {
			audit.Add(req.Name, "timer", formatDuration(req.Minutes))
		}
	}
	writeState(w, store)
}

type pauseReq struct {
	Minutes int `json:"minutes"`
}

func handlePause(w http.ResponseWriter, r *http.Request, store *Store, audit *AuditLog) {
	var req pauseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Minutes <= 0 {
		http.Error(w, "minutes (>0) is required", http.StatusBadRequest)
		return
	}
	paused := store.PauseDevices(time.Duration(req.Minutes) * time.Minute)
	if len(paused) > 0 {
		audit.Add("устройства", "pause", fmt.Sprintf("пауза %s (%d устр.)", strings.TrimPrefix(formatDuration(req.Minutes), "на "), len(paused)))
	}
	writeState(w, store)
}

func handleResume(w http.ResponseWriter, r *http.Request, store *Store, audit *AuditLog) {
	resumed := store.ResumeDevices()
	if len(resumed) > 0 {
		audit.Add("устройства", "resume", fmt.Sprintf("пауза снята (%d устр.)", len(resumed)))
	}
	writeState(w, store)
}

type presetReq struct {
	Name string `json:"name"`
}

func handleApplyPreset(w http.ResponseWriter, r *http.Request, store *Store, audit *AuditLog) {
	var req presetReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	found, count := store.ApplyPreset(req.Name)
	if !found {
		http.Error(w, "preset not found", http.StatusNotFound)
		return
	}
	audit.Add("пресет "+req.Name, "preset", fmt.Sprintf("применён (%d правил)", count))
	writeState(w, store)
}

func formatDuration(minutes int) string {
	switch {
	case minutes < 60:
		return fmt.Sprintf("на %d мин", minutes)
	case minutes%60 == 0:
		return fmt.Sprintf("на %d ч", minutes/60)
	default:
		return fmt.Sprintf("на %dч %dм", minutes/60, minutes%60)
	}
}

type addParamReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind,omitempty"`
	Group       string `json:"group,omitempty"`
	Inverted    bool   `json:"inverted,omitempty"` // pre-kind clients
}

func handleAddParam(w http.ResponseWriter, r *http.Request, store *Store, audit *AuditLog) {
	var req addParamReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.Kind == "" && req.Inverted {
		req.Kind = KindDevice
	}
	store.AddParam(req.Name, req.Description, req.Kind, req.Group)
	audit.Add(req.Name, "add", "создан")
	writeState(w, store)
}

func handleDeleteParam(w http.ResponseWriter, r *http.Request, store *Store, audit *AuditLog) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name query param is required", http.StatusBadRequest)
		return
	}
	if !store.DeleteParam(name) {
		http.Error(w, "param not found", http.StatusNotFound)
		return
	}
	audit.Add(name, "delete", "удалён")
	writeState(w, store)
}

// handleGetTemplates returns the catalog with per-item imported status.
func handleGetTemplates(w http.ResponseWriter, r *http.Request, store *Store) {
	params := store.GetState().Params
	type tplInfo struct {
		ServiceTemplate
		Imported bool `json:"imported"`
	}
	out := make([]tplInfo, 0, len(templateCatalog))
	for _, t := range templateCatalog {
		_, imported := params[t.ID]
		out = append(out, tplInfo{t, imported})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type importTemplatesReq struct {
	IDs []string `json:"ids"` // empty = all
}

// handleImportTemplates creates params for the selected templates.
// Existing params keep their state (AddParam re-classifies).
func handleImportTemplates(w http.ResponseWriter, r *http.Request, store *Store, audit *AuditLog) {
	var req importTemplatesReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	templates := selectTemplates(req.IDs)
	if len(templates) == 0 {
		http.Error(w, "no matching templates", http.StatusNotFound)
		return
	}
	for _, t := range templates {
		store.AddParam(t.ID, t.Title, KindService, t.Group)
	}
	audit.Add("шаблоны", "import", fmt.Sprintf("импортировано %d сервисов", len(templates)))
	writeState(w, store)
}

// handleTemplatesRsc serves the RouterOS import script.
// ?imported=1 — only templates imported into the app (router auto-import),
// ?ids=a,b,c — explicit filter, otherwise the whole catalog.
func handleTemplatesRsc(w http.ResponseWriter, r *http.Request, store *Store) {
	var templates []ServiceTemplate
	if r.URL.Query().Get("imported") != "" {
		templates = importedTemplates(store.GetState().Params)
	} else {
		var ids []string
		if q := r.URL.Query().Get("ids"); q != "" {
			ids = strings.Split(q, ",")
		}
		templates = selectTemplates(ids)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, renderTemplatesRsc(templates))
}

type addGroupReq struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func handleAddGroup(w http.ResponseWriter, r *http.Request, store *Store) {
	var req addGroupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	store.AddGroup(req.ID, req.Title)
	writeState(w, store)
}

func handleDeleteGroup(w http.ResponseWriter, r *http.Request, store *Store) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id query param is required", http.StatusBadRequest)
		return
	}
	if !store.DeleteGroup(id) {
		http.Error(w, "group not found or not empty", http.StatusConflict)
		return
	}
	writeState(w, store)
}
