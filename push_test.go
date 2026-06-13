package main

import (
	"os"
	"path/filepath"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func TestNewPushManagerGeneratesKeys(t *testing.T) {
	dir := t.TempDir()
	pm, err := NewPushManager(dir)
	if err != nil {
		t.Fatalf("NewPushManager: %v", err)
	}
	if pm.PublicKey() == "" || pm.VAPIDPrivate == "" {
		t.Error("VAPID keys should be generated")
	}
	info, err := os.Stat(filepath.Join(dir, "push.json"))
	if err != nil {
		t.Fatalf("push.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("push.json perms = %o, want 0600", perm)
	}
}

func TestPushKeysPersist(t *testing.T) {
	dir := t.TempDir()
	pm1, _ := NewPushManager(dir)
	pm2, err := NewPushManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pm1.PublicKey() != pm2.PublicKey() || pm1.VAPIDPrivate != pm2.VAPIDPrivate {
		t.Error("VAPID keys should be stable across reload")
	}
}

func TestSubscribeAddsReplacesAndCounts(t *testing.T) {
	pm, _ := NewPushManager(t.TempDir())
	if pm.Count() != 0 {
		t.Fatalf("fresh manager Count = %d, want 0", pm.Count())
	}

	a := webpush.Subscription{Endpoint: "https://push.example/a"}
	b := webpush.Subscription{Endpoint: "https://push.example/b"}
	pm.Subscribe(a)
	pm.Subscribe(b)
	if pm.Count() != 2 {
		t.Errorf("Count = %d, want 2", pm.Count())
	}

	// Re-subscribing the same endpoint replaces, not appends.
	pm.Subscribe(webpush.Subscription{Endpoint: "https://push.example/a"})
	if pm.Count() != 2 {
		t.Errorf("Count after re-subscribe = %d, want 2", pm.Count())
	}
}

func TestUnsubscribe(t *testing.T) {
	pm, _ := NewPushManager(t.TempDir())
	pm.Subscribe(webpush.Subscription{Endpoint: "https://push.example/a"})
	pm.Subscribe(webpush.Subscription{Endpoint: "https://push.example/b"})

	if err := pm.Unsubscribe("https://push.example/a"); err != nil {
		t.Fatal(err)
	}
	if pm.Count() != 1 {
		t.Errorf("Count after unsubscribe = %d, want 1", pm.Count())
	}
	// Unsubscribing an unknown endpoint is a no-op, not an error.
	if err := pm.Unsubscribe("https://push.example/unknown"); err != nil {
		t.Errorf("unknown unsubscribe should be no-op, got %v", err)
	}
	if pm.Count() != 1 {
		t.Errorf("Count = %d, want 1", pm.Count())
	}
}

func TestSubscriptionsPersist(t *testing.T) {
	dir := t.TempDir()
	pm, _ := NewPushManager(dir)
	pm.Subscribe(webpush.Subscription{Endpoint: "https://push.example/a"})

	pm2, err := NewPushManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pm2.Count() != 1 {
		t.Errorf("subscription did not persist: Count = %d", pm2.Count())
	}
}

func TestSendWithNoSubscribersIsNoop(t *testing.T) {
	pm, _ := NewPushManager(t.TempDir())
	// Should return immediately without panicking or blocking.
	pm.Send(pushPayload{Title: "x", Body: "y"})
}
