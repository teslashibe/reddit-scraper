package redditscraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeWebshareServer simulates the Webshare /proxy/list/ endpoint
// with optional pagination. We don't hit the real Webshare API in
// tests because it requires a valid API token tied to a real plan.
func fakeWebshareServer(t *testing.T, pages [][]map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy/list/", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token test-token" {
			http.Error(w, "bad auth: "+got, http.StatusUnauthorized)
			return
		}
		pageStr := r.URL.Query().Get("page")
		page := 0
		if pageStr != "" {
			fmt.Sscanf(pageStr, "%d", &page)
		}
		if page < 0 || page >= len(pages) {
			http.Error(w, "no such page", http.StatusNotFound)
			return
		}
		var nextPtr *string
		if page+1 < len(pages) {
			next := r.Host + r.URL.Path + "?page=" + fmt.Sprint(page+1)
			next = "http://" + next
			nextPtr = &next
		}
		resp := map[string]any{
			"count":   100,
			"next":    nextPtr,
			"results": pages[page],
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

func TestWebshareProxiesHappyPath(t *testing.T) {
	server := fakeWebshareServer(t, [][]map[string]any{
		{
			{"username": "u1", "password": "p1", "proxy_address": "10.0.0.1", "port": 5000, "valid": true, "country_code": "US"},
			{"username": "u2", "password": "p2", "proxy_address": "10.0.0.2", "port": 5001, "valid": true, "country_code": "GB"},
			{"username": "u3", "password": "p3", "proxy_address": "10.0.0.3", "port": 5002, "valid": false, "country_code": "US"}, // dropped
		},
	})
	defer server.Close()

	urls, err := WebshareProxies(context.Background(), WebshareConfig{
		APIToken: "test-token",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("WebshareProxies error = %v", err)
	}
	if len(urls) != 2 {
		t.Fatalf("got %d proxies, want 2 (one is invalid and should be filtered): %v", len(urls), urls)
	}
	if urls[0] != "http://u1:p1@10.0.0.1:5000" {
		t.Errorf("urls[0] = %q, want http://u1:p1@10.0.0.1:5000", urls[0])
	}
	if urls[1] != "http://u2:p2@10.0.0.2:5001" {
		t.Errorf("urls[1] = %q, want http://u2:p2@10.0.0.2:5001", urls[1])
	}
}

func TestWebshareProxiesPagination(t *testing.T) {
	server := fakeWebshareServer(t, [][]map[string]any{
		{
			{"username": "u1", "password": "p1", "proxy_address": "10.0.0.1", "port": 5000, "valid": true, "country_code": "US"},
		},
		{
			{"username": "u2", "password": "p2", "proxy_address": "10.0.0.2", "port": 5001, "valid": true, "country_code": "US"},
		},
		{
			{"username": "u3", "password": "p3", "proxy_address": "10.0.0.3", "port": 5002, "valid": true, "country_code": "US"},
		},
	})
	defer server.Close()

	urls, err := WebshareProxies(context.Background(), WebshareConfig{
		APIToken: "test-token",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("WebshareProxies error = %v", err)
	}
	if len(urls) != 3 {
		t.Fatalf("expected 3 proxies across 3 pages, got %d: %v", len(urls), urls)
	}
}

func TestWebshareProxiesCountryFilter(t *testing.T) {
	server := fakeWebshareServer(t, [][]map[string]any{
		{
			{"username": "u1", "password": "p1", "proxy_address": "10.0.0.1", "port": 5000, "valid": true, "country_code": "US"},
			{"username": "u2", "password": "p2", "proxy_address": "10.0.0.2", "port": 5001, "valid": true, "country_code": "GB"},
			{"username": "u3", "password": "p3", "proxy_address": "10.0.0.3", "port": 5002, "valid": true, "country_code": "DE"},
		},
	})
	defer server.Close()

	urls, err := WebshareProxies(context.Background(), WebshareConfig{
		APIToken:  "test-token",
		BaseURL:   server.URL,
		Countries: []string{"us", "DE"}, // case-insensitive
	})
	if err != nil {
		t.Fatalf("WebshareProxies error = %v", err)
	}
	if len(urls) != 2 {
		t.Fatalf("expected 2 proxies (US + DE only), got %d: %v", len(urls), urls)
	}
	if !strings.Contains(urls[0], "10.0.0.1") || !strings.Contains(urls[1], "10.0.0.3") {
		t.Errorf("country filter wrong, got %v", urls)
	}
}

func TestWebshareProxiesAuthFailure(t *testing.T) {
	server := fakeWebshareServer(t, nil)
	defer server.Close()

	_, err := WebshareProxies(context.Background(), WebshareConfig{
		APIToken: "wrong-token",
		BaseURL:  server.URL,
	})
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got %v", err)
	}
}

func TestWebshareProxiesEmptyToken(t *testing.T) {
	_, err := WebshareProxies(context.Background(), WebshareConfig{APIToken: ""})
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
	if !strings.Contains(err.Error(), "APIToken") {
		t.Errorf("expected APIToken in error, got %v", err)
	}
}
