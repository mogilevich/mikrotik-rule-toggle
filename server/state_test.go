package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigrateV1State(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	v1 := `{"params":{
		"roblox":{"enabled":true,"description":"Roblox","inverted":false},
		"eva-laptop":{"enabled":false,"description":"Eva","inverted":true}
	}}`
	if err := os.WriteFile(path, []byte(v1), 0644); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st := s.GetState()
	if st.Params["roblox"].Kind != KindService {
		t.Errorf("roblox kind = %q, want service", st.Params["roblox"].Kind)
	}
	if st.Params["eva-laptop"].Kind != KindDevice {
		t.Errorf("eva-laptop kind = %q, want device", st.Params["eva-laptop"].Kind)
	}
	if !st.Params["eva-laptop"].Inverted {
		t.Error("eva-laptop must stay inverted after migration")
	}
	if len(st.Groups) == 0 || len(st.Presets) == 0 {
		t.Errorf("expected seeded groups and presets, got %d/%d", len(st.Groups), len(st.Presets))
	}
}

func TestGroupToggleAndTimer(t *testing.T) {
	s := newTestStore(t)
	s.AddParam("yt", "YouTube", KindService, "video")
	s.AddParam("tiktok", "TikTok", KindService, "video")
	s.AddParam("roblox", "Roblox", KindService, "games")
	s.AddParam("laptop", "Laptop", KindDevice, "")

	if n := s.SetGroup("video", true); n != 2 {
		t.Fatalf("SetGroup affected %d params, want 2", n)
	}
	st := s.GetState()
	if !st.Params["yt"].Enabled || !st.Params["tiktok"].Enabled {
		t.Error("video members must be enabled")
	}
	if st.Params["roblox"].Enabled || st.Params["laptop"].Enabled {
		t.Error("params outside the group must not change")
	}

	if n := s.TempReleaseGroup("video", 30*time.Minute); n != 2 {
		t.Fatalf("TempReleaseGroup affected %d params, want 2", n)
	}
	st = s.GetState()
	for _, name := range []string{"yt", "tiktok"} {
		p := st.Params[name]
		if p.Enabled {
			t.Errorf("%s must be released", name)
		}
		if p.TimerDuration == nil || p.RevertEnabled == nil || !*p.RevertEnabled {
			t.Errorf("%s must have pending timer reverting to enabled", name)
		}
	}
}

func TestTimerRevertDirection(t *testing.T) {
	s := newTestStore(t)
	s.AddParam("yt", "", KindService, "video")
	s.SetParam("yt", true)
	s.TempRelease("yt", time.Minute)
	s.ActivatePendingTimers()

	// Simulate expiry
	s.mu.Lock()
	p := s.data.Params["yt"]
	past := time.Now().Add(-time.Minute).Unix()
	p.DisabledUntil = &past
	s.data.Params["yt"] = p
	s.mu.Unlock()

	restored := s.RestoreExpired()
	if len(restored) != 1 || restored[0] != "yt" {
		t.Fatalf("restored = %v, want [yt]", restored)
	}
	if !s.GetState().Params["yt"].Enabled {
		t.Error("service must revert to blocked (enabled=true)")
	}
}

func TestPauseAndResumeDevices(t *testing.T) {
	s := newTestStore(t)
	s.AddParam("laptop", "", KindDevice, "")
	s.AddParam("tablet", "", KindDevice, "")
	s.AddParam("yt", "", KindService, "video")
	s.SetParam("laptop", true)  // unrestricted
	s.SetParam("tablet", false) // manually restricted

	paused := s.PauseDevices(30 * time.Minute)
	if len(paused) != 1 || paused[0] != "laptop" {
		t.Fatalf("paused = %v, want [laptop]", paused)
	}
	st := s.GetState()
	if st.Params["laptop"].Enabled {
		t.Error("paused device must be restricted")
	}
	if st.Params["yt"].Enabled {
		t.Error("pause must not touch services")
	}

	resumed := s.ResumeDevices()
	if len(resumed) != 1 || resumed[0] != "laptop" {
		t.Fatalf("resumed = %v, want [laptop]", resumed)
	}
	st = s.GetState()
	if !st.Params["laptop"].Enabled {
		t.Error("resumed device must be unrestricted")
	}
	if st.Params["tablet"].Enabled {
		t.Error("manually restricted device must stay restricted after resume")
	}
}

func TestApplyPreset(t *testing.T) {
	s := newTestStore(t)
	s.AddParam("roblox", "", KindService, "games")
	s.AddParam("yt", "", KindService, "video")
	s.AddParam("inst", "", KindService, "social")

	found, count := s.ApplyPreset("homework")
	if !found || count != 3 {
		t.Fatalf("ApplyPreset(homework) = (%v, %d), want (true, 3)", found, count)
	}
	st := s.GetState()
	for _, name := range []string{"roblox", "yt", "inst"} {
		if !st.Params[name].Enabled {
			t.Errorf("%s must be blocked by homework preset", name)
		}
	}

	found, count = s.ApplyPreset("free-hour")
	if !found || count != 3 {
		t.Fatalf("ApplyPreset(free-hour) = (%v, %d), want (true, 3)", found, count)
	}
	st = s.GetState()
	for _, name := range []string{"roblox", "yt", "inst"} {
		p := st.Params[name]
		if p.Enabled || p.TimerDuration == nil {
			t.Errorf("%s must be released with a pending timer", name)
		}
		if *p.TimerDuration != 3600 {
			t.Errorf("%s timer = %ds, want 3600", name, *p.TimerDuration)
		}
	}

	if found, _ := s.ApplyPreset("nope"); found {
		t.Error("unknown preset must not be found")
	}
}

func TestDeleteGroupOnlyWhenEmpty(t *testing.T) {
	s := newTestStore(t)
	s.AddParam("roblox", "", KindService, "games")
	if s.DeleteGroup("games") {
		t.Error("non-empty group must not be deleted")
	}
	s.DeleteParam("roblox")
	if !s.DeleteGroup("games") {
		t.Error("empty group must be deleted")
	}
}
