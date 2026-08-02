package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestAddParamPreservesStateOnReclassify(t *testing.T) {
	s := newTestStore(t)
	s.AddParam("roblox", "Roblox", KindService, "")
	s.SetParam("roblox", true)
	s.TempRelease("roblox", 30*time.Minute)

	s.AddParam("roblox", "Roblox game", KindService, "games")
	p := s.GetState().Params["roblox"]
	if p.Group != "games" || p.Description != "Roblox game" {
		t.Errorf("reclassify must update group/description, got %+v", p)
	}
	if p.Enabled || p.TimerDuration == nil {
		t.Errorf("reclassify must preserve enabled state and timers, got %+v", p)
	}
}

func TestTemplateCatalogSane(t *testing.T) {
	validGroups := map[string]bool{"games": true, "video": true, "social": true}
	seen := map[string]bool{}
	for _, tpl := range templateCatalog {
		if tpl.ID == "" || strings.ContainsAny(tpl.ID, " \"") {
			t.Errorf("bad template id %q", tpl.ID)
		}
		if seen[tpl.ID] {
			t.Errorf("duplicate template id %q", tpl.ID)
		}
		seen[tpl.ID] = true
		if !validGroups[tpl.Group] {
			t.Errorf("%s: unknown group %q", tpl.ID, tpl.Group)
		}
		if len(tpl.Domains) == 0 {
			t.Errorf("%s: no domains", tpl.ID)
		}
	}
}

func TestRenderTemplatesRsc(t *testing.T) {
	rsc := renderTemplatesRsc(selectTemplates([]string{"roblox"}))
	for _, want := range []string{
		`list=blocked-roblox address=roblox.com`,
		`address=128.116.0.0/17`,
		`src-address-list=kids-devices dst-address-list=blocked-roblox action=drop comment="hook:roblox" disabled=yes`,
		`/ip/dns/static remove [find where comment="hook:roblox"]`,
		`!src-address-list] src-address-list=kids-devices`,
		`/ip/dns/static add type=FWD name=roblox.com match-subdomain=yes forward-to=$dnsUp address-list=blocked-roblox comment="tpl:roblox"`,
		`:local dnsUp`,
		`comment="hook:roblox" && dst-address-list="blocked-roblox"`,
		`protocol=tcp dst-port=443 tls-host=*roblox.com action=drop comment="hook:roblox" disabled=yes`,
		`src-address-list=kids-devices protocol=udp dst-port=443 action=drop comment="tpl:quic-block"`,
	} {
		if !strings.Contains(rsc, want) {
			t.Errorf("rsc missing %q", want)
		}
	}
	if strings.Contains(rsc, "NXDOMAIN") {
		t.Error("templates must not create LAN-wide NXDOMAIN entries")
	}
	// harvesters must never carry the hook: tag — the toggle cycle would disable them
	for _, line := range strings.Split(rsc, "\n") {
		if strings.Contains(line, "/ip/dns/static add") && strings.Contains(line, "hook:") {
			t.Errorf("dns harvester tagged hook:, must be tpl: only: %s", line)
		}
	}
	if strings.Contains(rsc, "hook:youtube") {
		t.Error("filtered rsc must not contain other templates")
	}
	// every add is guarded for idempotency: top-level adds are inline behind
	// an :if guard, block-level adds are indented inside an :if block
	for _, line := range strings.Split(rsc, "\n") {
		if strings.HasPrefix(line, "/ip/") && strings.Contains(line, " add ") {
			t.Errorf("unguarded add: %s", line)
		}
	}
}

func TestImportTemplatesPreservesState(t *testing.T) {
	s := newTestStore(t)
	s.AddParam("roblox", "старое описание", KindService, "")
	s.SetParam("roblox", true)
	tpl := templateByID("roblox")
	s.AddParam(tpl.ID, tpl.Title, KindService, tpl.Group)
	p := s.GetState().Params["roblox"]
	if !p.Enabled {
		t.Error("import must not reset enabled state")
	}
	if p.Group != "games" || p.Description != "Roblox" {
		t.Errorf("import must set group/title, got %+v", p)
	}
}

func TestImportedTemplatesAndHash(t *testing.T) {
	params := map[string]Param{
		"roblox":    {Kind: KindService},
		"vpn-block": {Kind: KindService}, // not in catalog
	}
	imported := importedTemplates(params)
	if len(imported) != 1 || imported[0].ID != "roblox" {
		t.Fatalf("importedTemplates = %v, want [roblox]", imported)
	}

	h1 := templatesHash(imported)
	if len(h1) != 12 {
		t.Errorf("hash length = %d, want 12", len(h1))
	}
	if h2 := templatesHash(imported); h2 != h1 {
		t.Error("hash must be stable for the same set")
	}
	params["youtube"] = Param{Kind: KindService}
	if h3 := templatesHash(importedTemplates(params)); h3 == h1 {
		t.Error("hash must change when the imported set changes")
	}
	if templatesHash(nil) != "" {
		t.Error("empty set must produce empty hash (field omitted for router)")
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
