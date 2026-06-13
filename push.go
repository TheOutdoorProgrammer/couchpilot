package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Web Push notifications: the UI registers a push subscription (service
// worker + VAPID), and couchpilot pings it when a code review needs eyes.
// iOS only delivers web push to PWAs installed via add-to-homescreen, and the
// page must be served over HTTPS — both are the browser's rules, not ours.

type PushManager struct {
	mu   sync.Mutex
	path string

	VAPIDPublic   string                 `json:"vapidPublic"`
	VAPIDPrivate  string                 `json:"vapidPrivate"`
	Subscriptions []webpush.Subscription `json:"subscriptions"`
}

func NewPushManager(dataDir string) (*PushManager, error) {
	pm := &PushManager{path: filepath.Join(dataDir, "push.json")}
	data, err := os.ReadFile(pm.path)
	if err == nil {
		if err := json.Unmarshal(data, pm); err != nil {
			log.Printf("push: %s corrupt (%v); regenerating", pm.path, err)
		}
	}
	if pm.VAPIDPublic == "" || pm.VAPIDPrivate == "" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		pm.VAPIDPrivate, pm.VAPIDPublic = priv, pub
		pm.Subscriptions = nil // subscriptions are bound to the old key pair
		if err := pm.save(); err != nil {
			return nil, err
		}
	}
	return pm, nil
}

func (pm *PushManager) save() error {
	data, err := json.MarshalIndent(pm, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(pm.path), 0755)
	return os.WriteFile(pm.path, data, 0600)
}

func (pm *PushManager) PublicKey() string {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.VAPIDPublic
}

// Subscribe stores a browser's push subscription, replacing any previous one
// for the same endpoint.
func (pm *PushManager) Subscribe(sub webpush.Subscription) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for i, s := range pm.Subscriptions {
		if s.Endpoint == sub.Endpoint {
			pm.Subscriptions[i] = sub
			return pm.save()
		}
	}
	pm.Subscriptions = append(pm.Subscriptions, sub)
	return pm.save()
}

func (pm *PushManager) Unsubscribe(endpoint string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for i, s := range pm.Subscriptions {
		if s.Endpoint == endpoint {
			pm.Subscriptions = append(pm.Subscriptions[:i], pm.Subscriptions[i+1:]...)
			return pm.save()
		}
	}
	return nil
}

func (pm *PushManager) Count() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return len(pm.Subscriptions)
}

type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"` // app-relative deep link
	Tag   string `json:"tag"` // collapses repeat notifications for one review
}

// Send fans the payload out to every subscription, asynchronously. Endpoints
// that report the subscription gone (404/410) are dropped.
func (pm *PushManager) Send(p pushPayload) {
	pm.mu.Lock()
	subs := make([]webpush.Subscription, len(pm.Subscriptions))
	copy(subs, pm.Subscriptions)
	priv, pub := pm.VAPIDPrivate, pm.VAPIDPublic
	pm.mu.Unlock()
	if len(subs) == 0 {
		return
	}

	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	for _, sub := range subs {
		go func(sub webpush.Subscription) {
			resp, err := webpush.SendNotification(data, &sub, &webpush.Options{
				Subscriber:      "https://github.com/TheOutdoorProgrammer/couchpilot",
				VAPIDPublicKey:  pub,
				VAPIDPrivateKey: priv,
				TTL:             3600,
				Urgency:         webpush.UrgencyHigh,
			})
			if err != nil {
				log.Printf("push: send: %v", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == 404 || resp.StatusCode == 410 {
				log.Printf("push: subscription gone (%d); dropping", resp.StatusCode)
				pm.Unsubscribe(sub.Endpoint)
			}
		}(sub)
	}
}
