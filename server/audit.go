package main

import (
	"encoding/json"
	"log"
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
	mu      sync.RWMutex
	path    string
	entries []AuditEntry
	maxSize int
	dirty   bool
	done    chan struct{}
}

func NewAuditLog(path string, maxSize int) *AuditLog {
	a := &AuditLog{
		path:    path,
		maxSize: maxSize,
		done:    make(chan struct{}),
	}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &a.entries)
	}
	go a.flushLoop()
	return a
}

func (a *AuditLog) flushLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.mu.Lock()
			if a.dirty {
				a.save()
				a.dirty = false
			}
			a.mu.Unlock()
		case <-a.done:
			return
		}
	}
}

// Flush writes pending entries to disk and stops the background loop.
func (a *AuditLog) Flush() {
	close(a.done)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dirty {
		a.save()
		a.dirty = false
	}
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
	if len(a.entries) > a.maxSize {
		a.entries = a.entries[len(a.entries)-a.maxSize:]
	}
	a.dirty = true
}

func (a *AuditLog) Recent(n int) []AuditEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if n > len(a.entries) {
		n = len(a.entries)
	}
	result := make([]AuditEntry, n)
	for i := 0; i < n; i++ {
		result[i] = a.entries[len(a.entries)-1-i]
	}
	return result
}

type ParamStats struct {
	Toggles  int            `json:"toggles"`
	Timers   int            `json:"timers"`
	Expired  int            `json:"expired"`
	LastUsed *int64         `json:"last_used"`
	TopTimer map[string]int `json:"top_timer"`
}

type Stats struct {
	Total   int                   `json:"total"`
	ByParam map[string]ParamStats `json:"by_param"`
}

func (a *AuditLog) Stats() Stats {
	a.mu.RLock()
	defer a.mu.RUnlock()

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

// DailyCount holds timer count for a single day.
type DailyCount struct {
	Date  string `json:"date"`  // "2006-01-02"
	Count int    `json:"count"`
}

// ParamDaily holds daily timer usage + total minutes for a param.
type ParamDaily struct {
	Days         []DailyCount `json:"days"`
	TotalTimers  int          `json:"total_timers"`
	TotalMinutes int          `json:"total_minutes"` // sum of timer durations
}

// FilteredStats is the response for the analytics endpoint.
type FilteredStats struct {
	ByParam map[string]ParamDaily `json:"by_param"`
}

// StatsFiltered returns daily timer breakdown for the last N days.
func (a *AuditLog) StatsFiltered(days int) FilteredStats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	now := time.Now()
	loc := now.Location()
	cutoff := now.AddDate(0, 0, -days).Unix()

	// date → param → count
	daily := make(map[string]map[string]int)
	totalTimers := make(map[string]int)
	totalMinutes := make(map[string]int)

	for _, e := range a.entries {
		if e.Time < cutoff {
			continue
		}
		if e.Action != "timer" {
			continue
		}
		day := time.Unix(e.Time, 0).In(loc).Format("2006-01-02")
		if daily[day] == nil {
			daily[day] = make(map[string]int)
		}
		daily[day][e.Param]++
		totalTimers[e.Param]++
		totalMinutes[e.Param] += parseMinutesFromDetail(e.Detail)
	}

	// Collect all param names
	paramSet := make(map[string]bool)
	for _, byParam := range daily {
		for p := range byParam {
			paramSet[p] = true
		}
	}

	// Build ordered day list
	today := now.In(loc)
	dayList := make([]string, days)
	for i := 0; i < days; i++ {
		dayList[i] = today.AddDate(0, 0, -(days-1-i)).Format("2006-01-02")
	}

	result := FilteredStats{ByParam: make(map[string]ParamDaily)}
	for p := range paramSet {
		pd := ParamDaily{
			Days:         make([]DailyCount, len(dayList)),
			TotalTimers:  totalTimers[p],
			TotalMinutes: totalMinutes[p],
		}
		for i, d := range dayList {
			c := 0
			if daily[d] != nil {
				c = daily[d][p]
			}
			pd.Days[i] = DailyCount{Date: d, Count: c}
		}
		result.ByParam[p] = pd
	}

	return result
}

// parseMinutesFromDetail extracts minutes from audit detail like "на 30 мин", "+на 1ч", "на 1ч 30м".
func parseMinutesFromDetail(detail string) int {
	total := 0
	num := 0
	for _, r := range detail {
		if r >= '0' && r <= '9' {
			num = num*10 + int(r-'0')
		} else if r == 'ч' {
			total += num * 60
			num = 0
		}
	}
	total += num
	return total
}

// save writes to disk. Must be called with mu held.
func (a *AuditLog) save() {
	raw, err := json.MarshalIndent(a.entries, "", "  ")
	if err != nil {
		log.Printf("ERROR: failed to marshal audit log: %v", err)
		return
	}
	if err := os.WriteFile(a.path, raw, 0644); err != nil {
		log.Printf("ERROR: failed to write %s: %v", a.path, err)
	}
}
