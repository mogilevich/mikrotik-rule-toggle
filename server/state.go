package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// Param kinds
const (
	KindService = "service" // firewall/DNS block: enabled = blocked
	KindDevice  = "device"  // kid-control: enabled = unrestricted (inverted on MikroTik)
)

type Param struct {
	Enabled       bool   `json:"enabled"`
	Description   string `json:"description"`
	Kind          string `json:"kind"`                     // KindService | KindDevice
	Group         string `json:"group,omitempty"`          // group id, services only
	Inverted      bool   `json:"inverted"`                 // kept in sync with Kind for older clients/rollback
	DisabledUntil *int64 `json:"disabled_until,omitempty"` // unix timestamp, nil = no timer
	TimerDuration *int64 `json:"timer_duration,omitempty"` // seconds, set while waiting for router to fetch
	RevertEnabled *bool  `json:"revert_enabled,omitempty"` // enabled value to restore when timer expires
}

// inverted reports whether the param has kid-control (inverted) logic.
func (p Param) inverted() bool { return p.Kind == KindDevice }

// blockedEnabled is the "restrictions active" value of Enabled for this param.
func (p Param) blockedEnabled() bool { return !p.inverted() }

type Group struct {
	Title string `json:"title,omitempty"` // empty = UI translates well-known ids (games/video/social)
	Order int    `json:"order"`
}

// PresetAction targets a single param or a whole group.
// Minutes > 0 → temporary release timer, otherwise a plain toggle to Enabled.
type PresetAction struct {
	Param   string `json:"param,omitempty"`
	Group   string `json:"group,omitempty"`
	Enabled bool   `json:"enabled"`
	Minutes int    `json:"minutes,omitempty"`
}

type Preset struct {
	Title   string         `json:"title,omitempty"` // empty = UI translates well-known ids
	Order   int            `json:"order"`
	Actions []PresetAction `json:"actions"`
}

type State struct {
	Params  map[string]Param  `json:"params"`
	Groups  map[string]Group  `json:"groups"`
	Presets map[string]Preset `json:"presets"`
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
	s.migrate()
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &s.data)
}

// migrate fills fields introduced after v1 state files and seeds defaults.
func (s *Store) migrate() {
	changed := false
	for k, p := range s.data.Params {
		if p.Kind == "" {
			if p.Inverted {
				p.Kind = KindDevice
			} else {
				p.Kind = KindService
			}
			changed = true
		}
		if p.Inverted != p.inverted() {
			p.Inverted = p.inverted()
			changed = true
		}
		s.data.Params[k] = p
	}
	if s.data.Groups == nil {
		s.data.Groups = map[string]Group{
			"games":  {Order: 1},
			"video":  {Order: 2},
			"social": {Order: 3},
		}
		changed = true
	}
	if s.data.Presets == nil {
		s.data.Presets = map[string]Preset{
			"homework": {Order: 1, Actions: []PresetAction{
				{Group: "games", Enabled: true},
				{Group: "video", Enabled: true},
				{Group: "social", Enabled: true},
			}},
			"free-hour": {Order: 2, Actions: []PresetAction{
				{Group: "games", Minutes: 60},
				{Group: "video", Minutes: 60},
				{Group: "social", Minutes: 60},
			}},
		}
		changed = true
	}
	if changed {
		s.save()
	}
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
	cp := State{
		Params:  make(map[string]Param, len(s.data.Params)),
		Groups:  make(map[string]Group, len(s.data.Groups)),
		Presets: make(map[string]Preset, len(s.data.Presets)),
	}
	for k, v := range s.data.Params {
		cp.Params[k] = v
	}
	for k, v := range s.data.Groups {
		cp.Groups[k] = v
	}
	for k, v := range s.data.Presets {
		cp.Presets[k] = v
	}
	return cp
}

// setParamLocked toggles a param and clears any timer. Caller holds s.mu.
func (s *Store) setParamLocked(name string, enabled bool) bool {
	p, ok := s.data.Params[name]
	if !ok {
		return false
	}
	p.Enabled = enabled
	p.DisabledUntil = nil // clear timer on manual toggle
	p.TimerDuration = nil
	p.RevertEnabled = nil
	s.data.Params[name] = p
	return true
}

func (s *Store) SetParam(name string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.setParamLocked(name, enabled) {
		return false
	}
	s.save()
	return true
}

// groupMembersLocked returns names of service params in a group. Caller holds s.mu.
func (s *Store) groupMembersLocked(group string) []string {
	var names []string
	for k, p := range s.data.Params {
		if p.Kind == KindService && p.Group == group {
			names = append(names, k)
		}
	}
	return names
}

// SetGroup toggles every service param in the group. Returns the number of params changed.
func (s *Store) SetGroup(group string, enabled bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	members := s.groupMembersLocked(group)
	for _, name := range members {
		s.setParamLocked(name, enabled)
	}
	if len(members) > 0 {
		s.save()
	}
	return len(members)
}

// tempReleaseLocked implements TempRelease for one param. Caller holds s.mu.
// Returns (found, extended).
func (s *Store) tempReleaseLocked(name string, durSec int64) (bool, bool) {
	p, ok := s.data.Params[name]
	if !ok {
		return false, false
	}
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
		// New timer — release restrictions, revert to blocked state on expiry
		revert := p.blockedEnabled()
		p.Enabled = !revert
		p.TimerDuration = &durSec
		p.RevertEnabled = &revert
	}

	s.data.Params[name] = p
	return true, extended
}

// TempRelease temporarily releases restrictions for a param.
// If a timer is already active (disabled_until set), extends it by dur.
// If a timer is pending (timer_duration set), adds dur to pending duration.
// Otherwise creates a new pending timer and records the state to revert to.
// Returns (found, extended).
func (s *Store) TempRelease(name string, dur time.Duration) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	found, extended := s.tempReleaseLocked(name, int64(dur.Seconds()))
	if found {
		s.save()
	}
	return found, extended
}

// TempReleaseGroup runs TempRelease for every service param in the group.
// Returns the number of params affected.
func (s *Store) TempReleaseGroup(group string, dur time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, name := range s.groupMembersLocked(group) {
		if found, _ := s.tempReleaseLocked(name, int64(dur.Seconds())); found {
			count++
		}
	}
	if count > 0 {
		s.save()
	}
	return count
}

// PauseDevices restricts every currently-unrestricted device param for dur
// (pending timer; reverts to unrestricted on expiry). Returns affected names.
func (s *Store) PauseDevices(dur time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	durSec := int64(dur.Seconds())
	var paused []string
	for k, p := range s.data.Params {
		if p.Kind != KindDevice || !p.Enabled {
			continue // already restricted (or not a device) — leave as is
		}
		revert := true
		d := durSec
		p.Enabled = false
		p.DisabledUntil = nil
		p.TimerDuration = &d
		p.RevertEnabled = &revert
		s.data.Params[k] = p
		paused = append(paused, k)
	}
	if len(paused) > 0 {
		s.save()
	}
	return paused
}

// ResumeDevices lifts an active pause: devices restricted by a timer that
// reverts to unrestricted get unrestricted now. Returns affected names.
func (s *Store) ResumeDevices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var resumed []string
	for k, p := range s.data.Params {
		if p.Kind != KindDevice || p.Enabled {
			continue
		}
		if p.RevertEnabled == nil || !*p.RevertEnabled {
			continue // restricted manually, not by pause — leave as is
		}
		p.Enabled = true
		p.DisabledUntil = nil
		p.TimerDuration = nil
		p.RevertEnabled = nil
		s.data.Params[k] = p
		resumed = append(resumed, k)
	}
	if len(resumed) > 0 {
		s.save()
	}
	return resumed
}

// ApplyPreset executes all actions of a preset. Returns (found, affected count).
func (s *Store) ApplyPreset(name string) (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	preset, ok := s.data.Presets[name]
	if !ok {
		return false, 0
	}
	count := 0
	for _, a := range preset.Actions {
		var targets []string
		if a.Param != "" {
			targets = []string{a.Param}
		} else if a.Group != "" {
			targets = s.groupMembersLocked(a.Group)
		}
		for _, target := range targets {
			if a.Minutes > 0 {
				if found, _ := s.tempReleaseLocked(target, int64(a.Minutes)*60); found {
					count++
				}
			} else if s.setParamLocked(target, a.Enabled) {
				count++
			}
		}
	}
	if count > 0 {
		s.save()
	}
	return true, count
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
			if p.RevertEnabled != nil {
				p.Enabled = *p.RevertEnabled
			} else {
				p.Enabled = p.blockedEnabled() // pre-revert_enabled timers
			}
			p.DisabledUntil = nil
			p.RevertEnabled = nil
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

func (s *Store) AddParam(name, description, kind, group string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind != KindDevice {
		kind = KindService
	}
	if kind == KindDevice {
		group = "" // groups apply to services only
	}
	p := Param{Enabled: false, Description: description, Kind: kind, Group: group}
	p.Inverted = p.inverted()
	s.data.Params[name] = p
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

// AddGroup creates or renames a group.
func (s *Store) AddGroup(id, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.data.Groups[id]
	if !ok {
		g.Order = len(s.data.Groups) + 1
	}
	g.Title = title
	s.data.Groups[id] = g
	s.save()
}

// DeleteGroup removes an empty group. Returns false if missing or non-empty.
func (s *Store) DeleteGroup(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Groups[id]; !ok {
		return false
	}
	if len(s.groupMembersLocked(id)) > 0 {
		return false
	}
	delete(s.data.Groups, id)
	s.save()
	return true
}
