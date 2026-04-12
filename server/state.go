package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

type Param struct {
	Enabled       bool   `json:"enabled"`
	Description   string `json:"description"`
	Inverted      bool   `json:"inverted"`                  // true = kid-control style (enabled in web → disabled on MikroTik)
	DisabledUntil *int64 `json:"disabled_until,omitempty"`  // unix timestamp, nil = no timer
	TimerDuration *int64 `json:"timer_duration,omitempty"`  // seconds, set while waiting for router to fetch
}

type State struct {
	Params map[string]Param `json:"params"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	data State
}

func NewStore(path string) (*Store, error) {
	s := &Store{
		path: path,
		data: State{Params: make(map[string]Param)},
	}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &s.data)
}

func (s *Store) save() {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		log.Printf("ERROR: failed to marshal state: %v", err)
		return
	}
	if err := os.WriteFile(s.path, raw, 0644); err != nil {
		log.Printf("ERROR: failed to write %s: %v", s.path, err)
	}
}

func (s *Store) GetState() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := State{Params: make(map[string]Param, len(s.data.Params))}
	for k, v := range s.data.Params {
		cp.Params[k] = v
	}
	return cp
}

func (s *Store) SetParam(name string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Params[name]
	if !ok {
		return false
	}
	p.Enabled = enabled
	p.DisabledUntil = nil  // clear timer on manual toggle
	p.TimerDuration = nil
	s.data.Params[name] = p
	s.save()
	return true
}

// TempRelease temporarily releases restrictions for a param.
// If a timer is already active (disabled_until set), extends it by dur.
// If a timer is pending (timer_duration set), adds dur to pending duration.
// Otherwise creates a new pending timer.
// Normal params:   sets enabled=false (unblock), reverts to enabled=true on expiry.
// Inverted params: sets enabled=true (unrestrict), reverts to enabled=false on expiry.
// Returns (found, extended).
func (s *Store) TempRelease(name string, dur time.Duration) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Params[name]
	if !ok {
		return false, false
	}
	durSec := int64(dur.Seconds())
	extended := false

	if p.DisabledUntil != nil {
		// Active timer — extend directly (router already applied)
		*p.DisabledUntil += durSec
		extended = true
	} else if p.TimerDuration != nil {
		// Pending timer — add to pending duration
		*p.TimerDuration += durSec
		extended = true
	} else {
		// New timer — toggle state and set pending
		if p.Inverted {
			p.Enabled = true
		} else {
			p.Enabled = false
		}
		p.TimerDuration = &durSec
	}

	s.data.Params[name] = p
	s.save()
	return true, extended
}

// ActivatePendingTimers converts pending timers (timer_duration) into active
// countdowns (disabled_until). Called when the router fetches state.
func (s *Store) ActivatePendingTimers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	now := time.Now()
	for k, p := range s.data.Params {
		if p.TimerDuration != nil {
			dur := *p.TimerDuration
			until := now.Add(time.Duration(dur) * time.Second).Unix()
			p.DisabledUntil = &until
			p.TimerDuration = nil
			s.data.Params[k] = p
			changed = true
			log.Printf("timer activated: %s (%ds)", k, dur)
		}
	}
	if changed {
		s.save()
	}
}

// RestoreExpired checks all params and restores those whose timer has expired.
// Returns names of restored params for audit logging.
func (s *Store) RestoreExpired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	changed := false
	var restored []string
	for k, p := range s.data.Params {
		if p.DisabledUntil != nil && *p.DisabledUntil <= now {
			if p.Inverted {
				p.Enabled = false
			} else {
				p.Enabled = true
			}
			p.DisabledUntil = nil
			s.data.Params[k] = p
			changed = true
			restored = append(restored, k)
			log.Printf("timer expired: restored %s", k)
		}
	}
	if changed {
		s.save()
	}
	return restored
}

func (s *Store) AddParam(name, description string, inverted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Params[name] = Param{Enabled: false, Description: description, Inverted: inverted}
	s.save()
}

func (s *Store) DeleteParam(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Params[name]; !ok {
		return false
	}
	delete(s.data.Params, name)
	s.save()
	return true
}
