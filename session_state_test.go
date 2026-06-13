package main

import (
	"errors"
	"os"
	"testing"
	"time"
)

func newStateManager(t *testing.T) *SessionManager {
	t.Helper()
	return &SessionManager{sessions: map[string]*Session{}, hub: NewSSEHub(), dataDir: t.TempDir()}
}

func TestSetReviewModeAndLookup(t *testing.T) {
	sm := newStateManager(t)
	sm.sessions["s1"] = &Session{ID: "s1", Status: StatusActive}

	if sm.ReviewModeOn("s1") {
		t.Error("review mode should default off")
	}
	if err := sm.SetReviewMode("s1", true); err != nil {
		t.Fatal(err)
	}
	if !sm.ReviewModeOn("s1") {
		t.Error("review mode should be on after SetReviewMode(true)")
	}
	if err := sm.SetReviewMode("missing", true); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("SetReviewMode on missing session = %v, want ErrNotExist", err)
	}
	if sm.ReviewModeOn("missing") {
		t.Error("ReviewModeOn for missing session should be false")
	}
}

func TestGetSessionsNewestFirst(t *testing.T) {
	sm := newStateManager(t)
	now := time.Now()
	sm.sessions["old"] = &Session{ID: "old", CreatedAt: now.Add(-time.Hour)}
	sm.sessions["new"] = &Session{ID: "new", CreatedAt: now}
	sm.sessions["mid"] = &Session{ID: "mid", CreatedAt: now.Add(-30 * time.Minute)}

	got := sm.GetSessions()
	if len(got) != 3 {
		t.Fatalf("got %d sessions", len(got))
	}
	if got[0].ID != "new" || got[1].ID != "mid" || got[2].ID != "old" {
		t.Errorf("wrong order: %s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestDismissSessionOnlyRemovesDead(t *testing.T) {
	sm := newStateManager(t)
	sm.sessions["alive"] = &Session{ID: "alive", Status: StatusActive}
	sm.sessions["dead"] = &Session{ID: "dead", Status: StatusDead}

	sm.DismissSession("alive")
	if _, ok := sm.sessions["alive"]; !ok {
		t.Error("dismissing an active session should be a no-op")
	}
	sm.DismissSession("dead")
	if _, ok := sm.sessions["dead"]; ok {
		t.Error("dead session should be removed on dismiss")
	}
}

func TestSetDiscard(t *testing.T) {
	sm := newStateManager(t)
	sm.sessions["s1"] = &Session{ID: "s1", Status: StatusActive}
	sm.SetDiscard("s1")
	if !sm.sessions["s1"].Discard {
		t.Error("Discard flag not set")
	}
}

func TestMarkDead(t *testing.T) {
	sm := newStateManager(t)
	sm.sessions["s1"] = &Session{ID: "s1", Status: StatusActive}

	var notified string
	sm.onSessionDied = func(id string) { notified = id }

	sm.markDead("s1")
	s := sm.sessions["s1"]
	if s.Status != StatusDead {
		t.Errorf("status = %s, want dead", s.Status)
	}
	if s.DiedAt == nil {
		t.Error("DiedAt should be set")
	}
	if notified != "s1" {
		t.Errorf("onSessionDied called with %q, want s1", notified)
	}

	// Marking an already-dead session must not re-fire the callback.
	notified = ""
	sm.markDead("s1")
	if notified != "" {
		t.Error("markDead on an already-dead session should be a no-op")
	}
}

func TestPersistAndLoadFromDisk(t *testing.T) {
	sm := newStateManager(t)
	sm.sessions["a"] = &Session{ID: "a", Name: "alpha", Status: StatusActive, SessionUUID: "ua"}
	sm.sessions["b"] = &Session{ID: "b", Name: "beta", Status: StatusDead}
	sm.persist()

	loaded, err := sm.loadFromDisk()
	if err != nil {
		t.Fatalf("loadFromDisk: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d, want 2", len(loaded))
	}
	byID := map[string]*Session{}
	for _, s := range loaded {
		byID[s.ID] = s
	}
	if byID["a"].Name != "alpha" || byID["a"].SessionUUID != "ua" {
		t.Errorf("session a not round-tripped: %+v", byID["a"])
	}
	if byID["b"].Status != StatusDead {
		t.Errorf("session b status = %s, want dead", byID["b"].Status)
	}
}

func TestKillSessionMissing(t *testing.T) {
	sm := newStateManager(t)
	if err := sm.KillSession("ghost"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("KillSession(missing) = %v, want ErrNotExist", err)
	}
}

func TestKillSessionAlreadyDeadIsNoop(t *testing.T) {
	sm := newStateManager(t)
	sm.sessions["d"] = &Session{ID: "d", Status: StatusDead}
	if err := sm.KillSession("d"); err != nil {
		t.Errorf("killing a dead session should be a no-op, got %v", err)
	}
}
