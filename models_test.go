package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const fakeCatalog = `{
  "models": {
    "anthropic/claude-opus-4-6": {"name": "Claude Opus 4.6", "family": "claude-opus", "release_date": "2026-02-01"},
    "anthropic/claude-haiku-4-5": {"name": "Claude Haiku 4.5", "family": "claude-haiku", "release_date": "2025-10-01"},
    "openai/gpt-5": {"name": "GPT-5", "family": "gpt", "release_date": "2026-03-01"},
    "google/gemini-3": {"name": "Gemini 3", "family": "gemini", "release_date": "2026-01-01"}
  }
}`

func TestFetchAnthropicModelsFiltersAndSorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fakeCatalog))
	}))
	defer srv.Close()

	old := modelsCatalogURL
	modelsCatalogURL = srv.URL
	defer func() { modelsCatalogURL = old }()

	got := fetchAnthropicModels()

	// Only anthropic/* entries survive, prefix-stripped.
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2 (anthropic only): %+v", len(got), got)
	}
	for _, m := range got {
		if m.ID == "" || m.ID[0] == 'g' { // gpt/gemini ids would start with g
			t.Errorf("non-anthropic model leaked: %+v", m)
		}
	}
	// Sorted newest-first by release date.
	if got[0].ID != "claude-opus-4-6" || got[1].ID != "claude-haiku-4-5" {
		t.Errorf("wrong sort order: %s then %s", got[0].ID, got[1].ID)
	}
	if got[0].Name != "Claude Opus 4.6" || got[0].Family != "claude-opus" {
		t.Errorf("metadata not mapped: %+v", got[0])
	}
}

func TestFetchAnthropicModelsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := modelsCatalogURL
	modelsCatalogURL = srv.URL
	defer func() { modelsCatalogURL = old }()

	if got := fetchAnthropicModels(); got != nil {
		t.Errorf("non-200 should yield nil, got %+v", got)
	}
}

func TestModelCacheServesStaleOnFetchFailure(t *testing.T) {
	mc := &modelCache{ttl: 0} // always considered expired
	mc.models = []ModelInfo{{ID: "cached"}}

	old := modelsCatalogURL
	modelsCatalogURL = "http://127.0.0.1:0" // unreachable
	defer func() { modelsCatalogURL = old }()

	got := mc.Get()
	if len(got) != 1 || got[0].ID != "cached" {
		t.Errorf("expired cache should fall back to last-known models, got %+v", got)
	}
}
