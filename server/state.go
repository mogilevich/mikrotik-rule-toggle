package main

import (
	"encoding/json"
	"os"
	"sync"
)

type Param struct {
	Enabled     bool   `json:"enabled"`
	Description string `json:"description"`
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
	s.data.Params[name] = p
	s.save()
	return true
}

func (s *Store) AddParam(name, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Params[name] = Param{Enabled: false, Description: description}
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
