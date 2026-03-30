package redditscraper

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL   = "https://www.reddit.com"
	defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	defaultTimeout   = 30 * time.Second
)

type client struct {
	httpClient    *http.Client
	baseURL       string
	userAgent     string
	authenticated bool

	mu             sync.Mutex
	remaining      float64
	resetAt        time.Time
	minRequestGap  time.Duration
	lastRequestAt  time.Time
	backoffUntil   time.Time
	consecutiveErr int
}

func newClient(opts *Options) *client {
	ua := defaultUserAgent
	if opts != nil && opts.UserAgent != "" {
		ua = opts.UserAgent
	}

	timeout := defaultTimeout
	if opts != nil && opts.RequestTimeout > 0 {
		timeout = opts.RequestTimeout
	}

	gap := 650 * time.Millisecond
	if opts != nil && opts.MinRequestGap > 0 {
		gap = opts.MinRequestGap
	}

	jar, _ := cookiejar.New(nil)

	return &client{
		httpClient: &http.Client{
			Timeout: timeout,
			Jar:     jar,
		},
		baseURL:       defaultBaseURL,
		userAgent:     ua,
		remaining:     100,
		minRequestGap: gap,
	}
}

func (c *client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", c.userAgent)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	return c.httpClient.Do(req)
}

func (c *client) get(path string) ([]byte, error) {
	c.waitForRateLimit()

	fullURL := c.baseURL + path
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.do(req)
	if err != nil {
		c.recordError()
		return nil, fmt.Errorf("executing request to %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	c.updateRateLimits(resp.Header)

	if resp.StatusCode == 429 {
		c.recordError()
		retryAfter := 60 * time.Second
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		time.Sleep(retryAfter)
		return c.get(path)
	}

	if resp.StatusCode != 200 {
		c.recordError()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d from %s: %s", resp.StatusCode, fullURL, truncateStr(string(body), 200))
	}

	c.consecutiveErr = 0
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return body, nil
}

// getHTML fetches a URL with text/html accept header (for shreddit endpoints).
func (c *client) getHTML(path string) ([]byte, error) {
	c.waitForRateLimit()

	fullURL := c.baseURL + path
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "text/vnd.reddit.partial+html, text/html")

	resp, err := c.do(req)
	if err != nil {
		c.recordError()
		return nil, fmt.Errorf("executing request to %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	c.updateRateLimits(resp.Header)

	if resp.StatusCode != 200 {
		c.recordError()
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, fullURL)
	}

	c.consecutiveErr = 0
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return body, nil
}

func (c *client) getJSON(path string, v interface{}) error {
	body, err := c.get(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decoding JSON from %s: %w (body preview: %s)", path, err, truncateStr(string(body), 200))
	}
	return nil
}

func (c *client) waitForRateLimit() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	if now.Before(c.backoffUntil) {
		time.Sleep(time.Until(c.backoffUntil))
	}

	if elapsed := now.Sub(c.lastRequestAt); elapsed < c.minRequestGap {
		time.Sleep(c.minRequestGap - elapsed)
	}

	if c.remaining <= 2 && time.Now().Before(c.resetAt) {
		wait := time.Until(c.resetAt) + time.Second
		time.Sleep(wait)
	}

	c.lastRequestAt = time.Now()
}

func (c *client) updateRateLimits(h http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if v := h.Get("X-Ratelimit-Remaining"); v != "" {
		v = strings.TrimSpace(v)
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.remaining = f
		}
	}
	if v := h.Get("X-Ratelimit-Reset"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			c.resetAt = time.Now().Add(time.Duration(secs) * time.Second)
		}
	}
}

func (c *client) recordError() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveErr++
	backoff := time.Duration(math.Min(
		float64(time.Duration(c.consecutiveErr)*5*time.Second),
		float64(2*time.Minute),
	))
	c.backoffUntil = time.Now().Add(backoff)
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
