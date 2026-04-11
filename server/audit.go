package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type AuditEntry struct {
	Time   int64  `json:"time"`   // unix timestamp
	Param  string `json:"param"`  // param name
	Action string `json:"action"` // toggle, timer, add, delete, expired
	Detail string `json:"detail"` // human-readable detail
}

type AuditLog struct {
	mu      sync.Mutex
	path    string
	entries []AuditEntry
	maxSize int
}

func NewAuditLog(path string, maxSize int) *AuditLog {
	a := &AuditLog{path: path, maxSize: maxSize}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &a.entries)
	}
	return a
}

func (a *AuditLog) Add(param, action, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, AuditEntry{
		Time:   time.Now().Unix(),
		Param:  param,
		Action: action,
		Detail: detail,
	})
	// Keep only last maxSize entries
	if len(a.entries) > a.maxSize {
		a.entries = a.entries[len(a.entries)-a.maxSize:]
	}
	a.save()
}

func (a *AuditLog) Recent(n int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n > len(a.entries) {
		n = len(a.entries)
	}
	// Return last n entries in reverse order (newest first)
	result := make([]AuditEntry, n)
	for i := 0; i < n; i++ {
		result[i] = a.entries[len(a.entries)-1-i]
	}
	return result
}

type ParamStats struct {
	Toggles  int            `json:"toggles"`  // total toggle count
	Timers   int            `json:"timers"`   // total timer count
	Expired  int            `json:"expired"`  // how many timers expired (not cancelled)
	LastUsed *int64         `json:"last_used"` // last action timestamp
	TopTimer map[string]int `json:"top_timer"` // timer duration → count
}

type Stats struct {
	Total    int                   `json:"total"`    // total events
	ByParam  map[string]ParamStats `json:"by_param"`
}

func (a *AuditLog) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()

	s := Stats{
		Total:   len(a.entries),
		ByParam: make(map[string]ParamStats),
	}

	for _, e := range a.entries {
		ps := s.ByParam[e.Param]
		if ps.TopTimer == nil {
			ps.TopTimer = make(map[string]int)
		}
		t := e.Time
		ps.LastUsed = &t

		switch e.Action {
		case "toggle":
			ps.Toggles++
		case "timer":
			ps.Timers++
			ps.TopTimer[e.Detail]++
		case "expired":
			ps.Expired++
		}

		s.ByParam[e.Param] = ps
	}

	return s
}

func (a *AuditLog) save() {
	raw, err := json.MarshalIndent(a.entries, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(a.path, raw, 0644)
}
