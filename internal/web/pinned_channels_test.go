package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// --- normalisePinnedList ---

func TestNormalisePinnedList(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"strips @", []string{"@chan1", "@chan2"}, []string{"chan1", "chan2"}},
		{"strips whitespace", []string{"  chan1  ", "\tchan2\t"}, []string{"chan1", "chan2"}},
		{"strips @ and whitespace combined", []string{" @chan1 "}, []string{"chan1"}},
		{"deduplicates", []string{"chan1", "chan1", "chan2"}, []string{"chan1", "chan2"}},
		{"dedup cross-format", []string{"@chan1", "chan1"}, []string{"chan1"}},
		{"drops empties", []string{"", "@", " ", "chan1", " @ "}, []string{"chan1"}},
		{"preserves order", []string{"z", "a", "m"}, []string{"z", "a", "m"}},
		{"nil input", nil, []string{}},
		{"empty input", []string{}, []string{}},
		{"x/ prefix preserved", []string{"x/handle1", "@x/handle2"}, []string{"x/handle1", "x/handle2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalisePinnedList(tc.in)
			if got == nil {
				got = []string{}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("normalisePinnedList(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// --- handlePinnedChannelToggle ---

func callPinToggle(t *testing.T, s *Server, channel string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"channel": channel})
	r := httptest.NewRequest(http.MethodPost, "/api/pinned-channels/toggle", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handlePinnedChannelToggle(w, r)
	if w.Code != 200 {
		t.Fatalf("POST /api/pinned-channels/toggle: status %d body %s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func TestPinToggleAddThenRemove(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		Profiles: []Profile{{ID: "p1", Nickname: "test", Config: Config{Domain: "example.com"}}},
		Active:   "p1",
	})

	// Pin a channel.
	res := callPinToggle(t, s, "mychannel")
	if res["pinned"] != true {
		t.Errorf("first toggle: pinned = %v, want true", res["pinned"])
	}
	if res["channel"] != "mychannel" {
		t.Errorf("channel = %v, want mychannel", res["channel"])
	}
	// Verify server-side.
	pl := loadProfilesT(t, s)
	if !reflect.DeepEqual(pl.Profiles[0].PinnedChannels, []string{"mychannel"}) {
		t.Errorf("PinnedChannels = %v, want [mychannel]", pl.Profiles[0].PinnedChannels)
	}

	// Unpin the same channel.
	res = callPinToggle(t, s, "mychannel")
	if res["pinned"] != false {
		t.Errorf("second toggle: pinned = %v, want false", res["pinned"])
	}
	pl = loadProfilesT(t, s)
	if len(pl.Profiles[0].PinnedChannels) != 0 {
		t.Errorf("PinnedChannels = %v, want empty", pl.Profiles[0].PinnedChannels)
	}
}

func TestPinToggleMultiplePins(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		Profiles: []Profile{{ID: "p1", Nickname: "test", Config: Config{Domain: "example.com"}}},
		Active:   "p1",
	})

	callPinToggle(t, s, "chan_a")
	callPinToggle(t, s, "chan_b")
	callPinToggle(t, s, "chan_c")

	pl := loadProfilesT(t, s)
	want := []string{"chan_a", "chan_b", "chan_c"}
	if !reflect.DeepEqual(pl.Profiles[0].PinnedChannels, want) {
		t.Errorf("PinnedChannels = %v, want %v", pl.Profiles[0].PinnedChannels, want)
	}

	// Remove middle one.
	callPinToggle(t, s, "chan_b")
	pl = loadProfilesT(t, s)
	want = []string{"chan_a", "chan_c"}
	if !reflect.DeepEqual(pl.Profiles[0].PinnedChannels, want) {
		t.Errorf("PinnedChannels = %v, want %v", pl.Profiles[0].PinnedChannels, want)
	}
}

func TestPinToggleStripsAt(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		Profiles: []Profile{{ID: "p1", Nickname: "test", Config: Config{Domain: "example.com"}}},
		Active:   "p1",
	})

	callPinToggle(t, s, "@mychannel")
	pl := loadProfilesT(t, s)
	if !reflect.DeepEqual(pl.Profiles[0].PinnedChannels, []string{"mychannel"}) {
		t.Errorf("PinnedChannels = %v, want [mychannel] (@ stripped)", pl.Profiles[0].PinnedChannels)
	}
}

func TestPinToggleXChannel(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		Profiles: []Profile{{ID: "p1", Nickname: "test", Config: Config{Domain: "example.com"}}},
		Active:   "p1",
	})

	callPinToggle(t, s, "x/elonmusk")
	pl := loadProfilesT(t, s)
	if !reflect.DeepEqual(pl.Profiles[0].PinnedChannels, []string{"x/elonmusk"}) {
		t.Errorf("PinnedChannels = %v, want [x/elonmusk]", pl.Profiles[0].PinnedChannels)
	}
}

// --- Profile update carry-over ---

func TestProfileUpdatePreservesPinnedChannels(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		Profiles: []Profile{{
			ID:             "p1",
			Nickname:       "test",
			Config:         Config{Domain: "example.com"},
			PinnedChannels: []string{"pinned1", "pinned2"},
			AutoUpdate:     []string{"auto1"},
		}},
		Active: "p1",
	})

	// Simulate what handleProfiles "update" does: load the profile,
	// build a new Profile from the request (without PinnedChannels),
	// then carry over PinnedChannels + AutoUpdate before saving.
	s.profilesMu.Lock()
	pl, err := s.loadProfilesExisting()
	if err != nil {
		s.profilesMu.Unlock()
		t.Fatalf("load: %v", err)
	}
	// Incoming update: new nickname, no PinnedChannels field.
	incoming := Profile{
		ID:       "p1",
		Nickname: "updated-test",
		Config:   Config{Domain: "example.com"},
	}
	for i, p := range pl.Profiles {
		if p.ID == incoming.ID {
			// This is the carry-over logic from handleProfiles.
			incoming.AutoUpdate = p.AutoUpdate
			incoming.AutoUpdateInterval = p.AutoUpdateInterval
			incoming.PinnedChannels = p.PinnedChannels
			pl.Profiles[i] = incoming
			break
		}
	}
	if err := s.saveProfiles(pl); err != nil {
		s.profilesMu.Unlock()
		t.Fatalf("save: %v", err)
	}
	s.profilesMu.Unlock()

	// Verify pins survived the update.
	pl = loadProfilesT(t, s)
	if !reflect.DeepEqual(pl.Profiles[0].PinnedChannels, []string{"pinned1", "pinned2"}) {
		t.Errorf("PinnedChannels = %v after update, want [pinned1 pinned2] (carry-over)", pl.Profiles[0].PinnedChannels)
	}
	if !reflect.DeepEqual(pl.Profiles[0].AutoUpdate, []string{"auto1"}) {
		t.Errorf("AutoUpdate = %v after update, want [auto1] (carry-over)", pl.Profiles[0].AutoUpdate)
	}
	if pl.Profiles[0].Nickname != "updated-test" {
		t.Errorf("Nickname = %q, want updated-test", pl.Profiles[0].Nickname)
	}
}
