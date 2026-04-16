package redditscraper

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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
	isProxied     bool

	mu             sync.Mutex
	remaining      float64
	resetAt        time.Time
	minRequestGap  time.Duration
	lastRequestAt  time.Time
	backoffUntil   time.Time
	consecutiveErr int
	dead           bool
}

func newClient(opts *Options) *client {
	return newClientWithProxy(opts, "")
}

func newClientWithProxy(opts *Options, proxyURL string) *client {
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

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
		if timeout > 10*time.Second {
			timeout = 10 * time.Second
		}
	}

	return &client{
		httpClient: &http.Client{
			Timeout:   timeout,
			Jar:       jar,
			Transport: transport,
		},
		baseURL:       defaultBaseURL,
		userAgent:     ua,
		remaining:     100,
		minRequestGap: gap,
		isProxied:     proxyURL != "",
	}
}

// clientPool distributes requests across multiple clients, each with
// independent rate-limit tracking. Picks the client with the most
// remaining rate-limit budget on each call.
type clientPool struct {
	clients []*client
	mu      sync.Mutex
	next    int
}

func newClientPool(opts *Options) *clientPool {
	pool := &clientPool{}

	// Always include a direct (no-proxy) client
	pool.clients = append(pool.clients, newClient(opts))

	if opts != nil && len(opts.ProxyURLs) > 0 {
		for _, proxy := range opts.ProxyURLs {
			pool.clients = append(pool.clients, newClientWithProxy(opts, proxy))
		}
	}

	return pool
}

// HealthCheck tests all proxied clients against a live URL and disables
// those that fail. The direct client (index 0) is never disabled.
func (p *clientPool) HealthCheck(testPath string) (healthy, dead int) {
	if len(p.clients) <= 1 {
		return 1, 0
	}

	type result struct {
		idx int
		ok  bool
	}
	ch := make(chan result, len(p.clients)-1)

	for i := 1; i < len(p.clients); i++ {
		go func(idx int) {
			c := p.clients[idx]
			fullURL := c.baseURL + testPath
			req, err := http.NewRequest("GET", fullURL, nil)
			if err != nil {
				ch <- result{idx, false}
				return
			}
			resp, err := c.do(req)
			if err != nil {
				ch <- result{idx, false}
				return
			}
			resp.Body.Close()
			ch <- result{idx, resp.StatusCode == 200}
		}(i)
	}

	for i := 1; i < len(p.clients); i++ {
		r := <-ch
		if !r.ok {
			p.clients[r.idx].mu.Lock()
			p.clients[r.idx].dead = true
			p.clients[r.idx].mu.Unlock()
			dead++
		} else {
			healthy++
		}
	}
	healthy++ // direct client
	return healthy, dead
}

func (p *clientPool) pick() *client {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.clients) == 1 {
		return p.clients[0]
	}

	best := p.next
	bestScore := -1000.0
	now := time.Now()
	for i, c := range p.clients {
		c.mu.Lock()
		if c.dead {
			c.mu.Unlock()
			continue
		}
		score := c.remaining
		if c.consecutiveErr > 0 {
			score -= float64(c.consecutiveErr) * 50
		}
		if now.Before(c.backoffUntil) {
			score -= 200
		}
		c.mu.Unlock()
		if score > bestScore {
			bestScore = score
			best = i
		}
	}
	p.next = (best + 1) % len(p.clients)
	return p.clients[best]
}

func (p *clientPool) poolRetries() int {
	n := len(p.clients)
	if n <= 1 {
		return 1
	}
	r := n / 3
	if r < 3 {
		r = 3
	}
	if r > 8 {
		r = 8
	}
	return r
}

func (p *clientPool) get(path string) ([]byte, error) {
	var lastErr error
	for i := 0; i < p.poolRetries(); i++ {
		body, err := p.pick().get(path)
		if err == nil {
			return body, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (p *clientPool) getJSON(path string, v interface{}) error {
	var lastErr error
	for i := 0; i < p.poolRetries(); i++ {
		err := p.pick().getJSON(path, v)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func (p *clientPool) getHTML(path string) ([]byte, error) {
	var lastErr error
	for i := 0; i < p.poolRetries(); i++ {
		body, err := p.pick().getHTML(path)
		if err == nil {
			return body, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (p *clientPool) size() int {
	return len(p.clients)
}

func (c *client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", c.userAgent)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	return c.httpClient.Do(req)
}

const maxRetries = 3

func (c *client) get(path string) ([]byte, error) {
	return c.getWithRetry(path, 0)
}

func (c *client) getWithRetry(path string, attempt int) ([]byte, error) {
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
		if c.isProxied || attempt >= maxRetries {
			return nil, fmt.Errorf("rate limited on %s", fullURL)
		}
		retryAfter := 60 * time.Second
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		time.Sleep(retryAfter)
		return c.getWithRetry(path, attempt+1)
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
	if c.consecutiveErr >= 5 {
		c.dead = true
	}
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
