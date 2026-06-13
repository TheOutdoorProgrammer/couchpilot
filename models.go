package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Family      string `json:"family"`
	ReleaseDate string `json:"releaseDate"`
}

type modelCache struct {
	mu        sync.RWMutex
	models    []ModelInfo
	fetchedAt time.Time
	ttl       time.Duration
}

var models = &modelCache{ttl: 24 * time.Hour}

// modelsCatalogURL is the upstream model metadata source. Kept as a var so
// tests can point the parser at a local server.
var modelsCatalogURL = "https://models.dev/catalog.json"

type catalogEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Family      string `json:"family"`
	ReleaseDate string `json:"release_date"`
}

func (mc *modelCache) Get() []ModelInfo {
	mc.mu.RLock()
	if len(mc.models) > 0 && time.Since(mc.fetchedAt) < mc.ttl {
		defer mc.mu.RUnlock()
		return mc.models
	}
	mc.mu.RUnlock()

	fetched := fetchAnthropicModels()
	if len(fetched) == 0 {
		mc.mu.RLock()
		defer mc.mu.RUnlock()
		return mc.models
	}

	mc.mu.Lock()
	mc.models = fetched
	mc.fetchedAt = time.Now()
	mc.mu.Unlock()
	return fetched
}

func fetchAnthropicModels() []ModelInfo {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(modelsCatalogURL)
	if err != nil {
		log.Printf("models: fetch failed: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("models: fetch returned %d", resp.StatusCode)
		return nil
	}

	var catalog struct {
		Models map[string]catalogEntry `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		log.Printf("models: decode failed: %v", err)
		return nil
	}

	var out []ModelInfo
	for id, entry := range catalog.Models {
		if !strings.HasPrefix(id, "anthropic/") {
			continue
		}
		modelID := strings.TrimPrefix(id, "anthropic/")
		out = append(out, ModelInfo{
			ID:          modelID,
			Name:        entry.Name,
			Family:      entry.Family,
			ReleaseDate: entry.ReleaseDate,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ReleaseDate > out[j].ReleaseDate
	})

	return out
}
