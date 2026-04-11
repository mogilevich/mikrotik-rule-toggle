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
	Inverted      bool   `json:"inverted"`                 // true = kid-control style (enabled in web → disabled on MikroTik)
	DisabledUntil *int64 `json:"disabled_until,omitempty"` // unix timestamp, nil = no timer
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

func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0644)
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
	p.DisabledUntil = nil // clear timer on manual toggle
	s.data.Params[name] = p
	s.save()
	return true
}

// TempRelease temporarily releases restrictions for a param.
// Normal params:   sets enabled=false (unblock), reverts to enabled=true on expiry.
// Inverted params: sets enabled=true (unrestrict), reverts to enabled=false on expiry.
func (s *Store) TempRelease(name string, dur time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Params[name]
	if !ok {
		return false
	}
	until := time.Now().Add(dur).Unix()
	if p.Inverted {
		p.Enabled = true // kid-control: enabled=true → restrictions off
	} else {
		p.Enabled = false // firewall: enabled=false → rule disabled
	}
	p.DisabledUntil = &until
	s.data.Params[name] = p
	s.save()
	return true
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
