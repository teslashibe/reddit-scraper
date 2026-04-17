package redditscraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Webshare (https://webshare.io) is a residential / datacenter proxy
// service. Their proxy list rotates server-side as bad IPs are
// retired and replaced, which means polling the list endpoint with a
// valid API token is enough to keep a "stable set of N healthy
// proxies" — no manual intervention required when individual proxies
// get banned by upstream targets like Reddit.
//
// This file ships a minimal GET-only adapter. We deliberately avoid
// the POST /proxy/list/refresh/ endpoint because not all plans
// include the "Replace IPs" feature and triggering it without
// authorisation can cost real money or hit per-day quotas.

// DefaultWebshareBaseURL is the standard production endpoint. Override
// via WebshareConfig.BaseURL for testing or for self-hosted mirrors.
const DefaultWebshareBaseURL = "https://proxy.webshare.io/api/v2"

// DefaultWebshareTimeout is the per-request timeout for Webshare API
// calls. Overrideable via WebshareConfig.HTTPClient.
const DefaultWebshareTimeout = 30 * time.Second

// WebshareConfig parameterises a single call to WebshareProxies.
// All fields except APIToken are optional and have sensible defaults.
type WebshareConfig struct {
	// APIToken is the Webshare API key from
	// https://proxy.webshare.io/userapi/keys/. Required.
	APIToken string

	// BaseURL overrides the default Webshare API root. Useful for
	// testing against a fake server. Empty = production.
	BaseURL string

	// PageSize is how many proxies to request per page (Webshare
	// caps this around 100). Empty / zero = 100.
	PageSize int

	// Mode selects Webshare's proxy mode: "direct" (rotating) or
	// "backbone" (static gateway). Empty = "direct" which matches
	// what the existing Webshare 100 proxies.txt file uses.
	Mode string

	// Countries optionally filters proxies by ISO country code
	// (e.g. ["US", "GB"]). Empty = no filter.
	Countries []string

	// HTTPClient overrides the default client (30s timeout). Useful
	// for tests or for routing the API call through a corporate
	// HTTP proxy (which is a different thing than Webshare's
	// proxy list, of course).
	HTTPClient *http.Client
}

type webshareListResponse struct {
	Count   int     `json:"count"`
	Next    *string `json:"next"`
	Results []struct {
		Username     string `json:"username"`
		Password     string `json:"password"`
		ProxyAddress string `json:"proxy_address"`
		Port         int    `json:"port"`
		Valid        bool   `json:"valid"`
		CountryCode  string `json:"country_code"`
	} `json:"results"`
}

// WebshareProxies fetches the current proxy list from the Webshare
// API and returns it in the URL form expected by Options.ProxyURLs.
//
// Typical use: call this fresh before each scrape job. Webshare
// retires banned IPs server-side so re-fetching the list yields a
// self-healing pool with no client-side bookkeeping. For
// long-running scrapes you can also call it periodically and pass
// the new list to a fresh Scraper instance.
//
// Pages through the entire list (Webshare paginates at ~100 per
// page) and filters out any proxies marked invalid. Returns
// (nil, error) on any HTTP failure or malformed response — the
// caller should fall back to a static proxy list in that case.
func WebshareProxies(ctx context.Context, cfg WebshareConfig) ([]string, error) {
	if strings.TrimSpace(cfg.APIToken) == "" {
		return nil, fmt.Errorf("webshare: APIToken is required")
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultWebshareBaseURL
	}
	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	mode := cfg.Mode
	if mode == "" {
		mode = "direct"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultWebshareTimeout}
	}

	countryFilter := make(map[string]bool, len(cfg.Countries))
	for _, c := range cfg.Countries {
		if c = strings.TrimSpace(c); c != "" {
			countryFilter[strings.ToUpper(c)] = true
		}
	}

	pageURL := fmt.Sprintf("%s/proxy/list/?mode=%s&page_size=%d", strings.TrimRight(baseURL, "/"), mode, pageSize)
	var urls []string
	for pageURL != "" {
		req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
		if err != nil {
			return nil, fmt.Errorf("webshare: build request: %w", err)
		}
		req.Header.Set("Authorization", "Token "+cfg.APIToken)
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("webshare: request: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("webshare: status %d: %s", resp.StatusCode, truncateStr(string(body), 200))
		}

		var page webshareListResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("webshare: decode: %w", err)
		}
		for _, r := range page.Results {
			if !r.Valid {
				continue
			}
			if len(countryFilter) > 0 && !countryFilter[strings.ToUpper(r.CountryCode)] {
				continue
			}
			urls = append(urls, fmt.Sprintf("http://%s:%s@%s:%d", r.Username, r.Password, r.ProxyAddress, r.Port))
		}

		if page.Next == nil {
			break
		}
		pageURL = *page.Next
	}

	return urls, nil
}
