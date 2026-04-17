package redditscraper

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// PostSource selects which backend FetchPosts uses for post discovery.
type PostSource string

const (
	// SourceAuto (default) tries Arctic Shift first, falls back to
	// Reddit's listing API on error. Best for "I want as many posts
	// as possible" workflows.
	SourceAuto PostSource = ""

	// SourceReddit uses only Reddit's public listing API. Capped at
	// ~2-5k posts per subreddit (the 1000-per-listing × multi-sort
	// dedup ceiling) but always live and authoritative.
	SourceReddit PostSource = "reddit"

	// SourceArctic uses only the Arctic Shift archive (community
	// Pushshift replacement at https://arctic-shift.photon-reddit.com).
	// Unlocks tens of thousands of historical posts but depends on a
	// third-party service and may lag the live site by hours.
	SourceArctic PostSource = "arctic"
)

// Options configures the scraper behaviour. All fields are optional.
type Options struct {
	// Token is a Reddit session token (token_v2 cookie value).
	// Get it from your browser DevTools → Application → Cookies → reddit.com.
	// When set, requests are sent as an authenticated user, which can
	// unlock NSFW subreddits and age-gated content.
	Token string

	// UserAgent overrides the default browser-like User-Agent string.
	UserAgent string

	// RequestTimeout is the HTTP client timeout per request (default 30s).
	RequestTimeout time.Duration

	// MinRequestGap is the minimum pause between consecutive HTTP requests
	// to stay under Reddit's rate limits (default 650ms).
	MinRequestGap time.Duration

	// PostLimit caps the number of posts returned. 0 means unlimited.
	PostLimit int

	// CommentDepth limits the nesting depth when fetching comment trees
	// (default 500). Reddit rarely nests deeper than ~20 levels, so the
	// default effectively means "get everything".
	CommentDepth int

	// SkipComments makes ScrapeAll skip comment fetching entirely,
	// returning only post metadata. Significantly faster for large scrapes.
	SkipComments bool

	// MoreCommentsTimeout caps the time spent expanding Reddit's "more"
	// comment tokens per post (default 60s). Hitting the budget returns a
	// partial-comments warning, not an error — ScrapeAll and FetchSinglePost
	// keep whatever comments were collected up to that point.
	//
	// Raise this for slow or heavily-proxied environments where each
	// expansion round-trip can cost several hundred milliseconds.
	MoreCommentsTimeout time.Duration

	// ProxyURLs is a list of HTTP/SOCKS5 proxy URLs to rotate through.
	// Each proxy gets its own rate-limit budget. Format: http://host:port,
	// http://user:pass@host:port, or socks5://host:port.
	ProxyURLs []string

	// Source selects the post-discovery backend. Default ("") tries
	// Arctic Shift first then falls back to Reddit. Comments are always
	// fetched live from Reddit regardless of this setting.
	Source PostSource

	// OldestPost is a server-side date cutoff: only posts created on
	// or after this timestamp are fetched. Zero value means no cutoff
	// (fetch full available history up to PostLimit). Honoured by both
	// the Arctic Shift and Reddit backends.
	OldestPost time.Time

	// ArcticAPIBase overrides the Arctic Shift base URL. Default is
	// https://arctic-shift.photon-reddit.com. Useful for testing or
	// pointing at a self-hosted mirror.
	ArcticAPIBase string

	// ArcticRequestGap is the minimum pause between Arctic Shift
	// requests. Default 200ms. Arctic Shift has no published rate
	// limit but we stay polite.
	ArcticRequestGap time.Duration

	// CommentWorkers controls comment-fetch concurrency in ScrapeAll.
	//
	//   0 or 1 → sequential (current behaviour: a single per-post
	//            error aborts the entire scrape)
	//   N >= 2 → parallel via N goroutines sharing the proxy pool;
	//            per-post errors are tolerated (logged via progress,
	//            the post simply has empty Comments) so transient
	//            failures don't kill huge scrapes
	//
	// Recommended: pass roughly one worker per healthy proxy in the
	// pool. The pool's per-client rate-limit gap (MinRequestGap)
	// naturally caps throughput per IP, so over-subscribing workers
	// just means a small wait queue per proxy — not request flooding.
	CommentWorkers int
}

const defaultMoreCommentsTimeout = 60 * time.Second

// Scraper fetches posts and comments from a subreddit.
type Scraper struct {
	client   *client
	pool     *clientPool
	opts     Options
	progress chan<- Progress
}

// New creates a Scraper with the given options and an optional progress
// channel. Pass nil for either argument to use defaults / disable progress.
func New(opts *Options, progress chan<- Progress) *Scraper {
	if opts == nil {
		opts = &Options{}
	}
	if opts.CommentDepth == 0 {
		opts.CommentDepth = 500
	}

	pool := newClientPool(opts)

	return &Scraper{
		client:   pool.clients[0],
		pool:     pool,
		opts:     *opts,
		progress: progress,
	}
}

// ProxyCount returns the number of clients in the pool (including direct).
func (s *Scraper) ProxyCount() int {
	return s.pool.size()
}

// HealthCheck tests all proxied clients and disables dead ones.
// Returns (healthy, dead) counts.
func (s *Scraper) HealthCheck(subreddit string) (int, int) {
	testPath := fmt.Sprintf("/r/%s/new.json?limit=1&raw_json=1", subreddit)
	return s.pool.HealthCheck(testPath)
}

// IsAuthenticated returns whether the scraper has a valid session.
func (s *Scraper) IsAuthenticated() bool {
	return s.client.IsAuthenticated()
}

// FetchSubreddit returns metadata for the named subreddit.
func (s *Scraper) FetchSubreddit(name string) (*Subreddit, error) {
	path := fmt.Sprintf("/r/%s/about.json", name)

	var raw struct {
		Data json.RawMessage `json:"data"`
	}
	if err := s.pool.getJSON(path, &raw); err != nil {
		return nil, fmt.Errorf("fetching subreddit info: %w", err)
	}

	var fields struct {
		DisplayName       string  `json:"display_name"`
		Title             string  `json:"title"`
		Description       string  `json:"description"`
		PublicDescription string  `json:"public_description"`
		Subscribers       int     `json:"subscribers"`
		CreatedUTC        float64 `json:"created_utc"`
		Over18            bool    `json:"over18"`
		SubredditType     string  `json:"subreddit_type"`
		ActiveUserCount   int     `json:"active_user_count"`
	}
	if err := json.Unmarshal(raw.Data, &fields); err != nil {
		return nil, fmt.Errorf("decoding subreddit data: %w", err)
	}

	return &Subreddit{
		Name:              fields.DisplayName,
		Title:             fields.Title,
		Description:       fields.Description,
		PublicDescription: fields.PublicDescription,
		Subscribers:       fields.Subscribers,
		CreatedUTC:        time.Unix(int64(fields.CreatedUTC), 0),
		Over18:            fields.Over18,
		SubredditType:     fields.SubredditType,
		ActiveUserCount:   fields.ActiveUserCount,
	}, nil
}

// FetchPosts collects posts from a subreddit. The backend used depends
// on Options.Source:
//
//   - SourceAuto (default): tries Arctic Shift first (10-30× more
//     historical data than Reddit's listing API), falls back to the
//     Reddit multi-sort strategy on error.
//   - SourceArctic: Arctic Shift only. Returns an error if Arctic
//     Shift is unreachable.
//   - SourceReddit: Reddit listing API only. Capped at ~2-5k posts
//     per subreddit due to Reddit's 1000-per-listing server-side
//     limit, but always live and authoritative.
//
// Posts are returned newest-first. Options.PostLimit and
// Options.OldestPost both act as stop conditions.
func (s *Scraper) FetchPosts(subreddit string) ([]Post, error) {
	switch s.opts.Source {
	case SourceArctic:
		return s.fetchPostsArctic(subreddit)
	case SourceReddit:
		return s.fetchPostsReddit(subreddit)
	default:
		// auto: try Arctic Shift first, fall back to Reddit on hard error
		posts, err := s.fetchPostsArctic(subreddit)
		if err == nil && len(posts) > 0 {
			return posts, nil
		}
		if err != nil {
			s.emit(Progress{
				Phase:   "posts",
				Message: fmt.Sprintf("arctic-shift failed (%v), falling back to reddit listings", err),
			})
		} else {
			s.emit(Progress{
				Phase:   "posts",
				Message: "arctic-shift returned no posts, falling back to reddit listings",
			})
		}
		return s.fetchPostsReddit(subreddit)
	}
}

// fetchPostsReddit is the legacy multi-sort strategy that walks
// Reddit's public listing endpoints. Each individual sort caps at
// ~1000 results server-side, so we merge several sort/time combos to
// maximise unique-post reach. Adding a sort here is essentially free
// — overlap with the others is dedup'd by the seen map.
func (s *Scraper) fetchPostsReddit(subreddit string) ([]Post, error) {
	seen := make(map[string]bool)
	var allPosts []Post

	type strategy struct{ sort, t string }
	strategies := []strategy{
		// Original 7 strategies — these alone got ~2-3k unique posts.
		{"new", ""},
		{"top", "all"},
		{"top", "year"},
		{"top", "month"},
		{"controversial", "all"},
		{"controversial", "year"},
		{"hot", ""},
		// Phase B (added 2026-04): on big subs these contribute another
		// ~1-2k unique posts that the originals miss. Free win — each
		// extra sort costs at most ~10 paginated requests.
		{"top", "week"},
		{"top", "day"},
		{"controversial", "month"},
		{"controversial", "week"},
		{"rising", ""},
	}

	for _, strat := range strategies {
		label := strat.sort
		if strat.t != "" {
			label += "/" + strat.t
		}

		posts, err := s.fetchListingPages(subreddit, strat.sort, strat.t, seen)
		if err != nil {
			s.emit(Progress{Phase: "posts", Message: fmt.Sprintf("warning: %s failed: %v", label, err)})
			continue
		}
		allPosts = append(allPosts, posts...)

		s.emit(Progress{
			Phase:        "posts",
			PostsFetched: len(allPosts),
			Message:      fmt.Sprintf("after %s: %d unique posts total (%d new from this sort)", label, len(allPosts), len(posts)),
		})

		if s.opts.PostLimit > 0 && len(allPosts) >= s.opts.PostLimit {
			break
		}
	}

	// Drop posts older than the cutoff (server-side filtering on
	// listing endpoints isn't supported, so we filter client-side).
	if !s.opts.OldestPost.IsZero() {
		cutoff := s.opts.OldestPost
		filtered := make([]Post, 0, len(allPosts))
		for _, p := range allPosts {
			if !p.CreatedUTC.Before(cutoff) {
				filtered = append(filtered, p)
			}
		}
		allPosts = filtered
	}

	sort.Slice(allPosts, func(i, j int) bool {
		return allPosts[i].CreatedUTC.After(allPosts[j].CreatedUTC)
	})

	if s.opts.PostLimit > 0 && len(allPosts) > s.opts.PostLimit {
		allPosts = allPosts[:s.opts.PostLimit]
	}

	return allPosts, nil
}

// FetchListingPages paginates a single sort/time listing endpoint,
// adding only posts not already in the seen set. Stops early after
// 3 consecutive pages with zero new posts to avoid wasting requests.
// The seen map is mutated and can be shared across calls to de-dupe
// across strategies.
func (s *Scraper) FetchListingPages(subreddit, sortOrder, timeFilter string, seen map[string]bool) ([]Post, error) {
	return s.fetchListingPages(subreddit, sortOrder, timeFilter, seen)
}

// FetchSearch paginates the subreddit's search endpoint. Reddit's
// search index returns posts matching the query, capped at 1000
// results per query. Combining queries (different keywords, flairs,
// or time buckets) unlocks additional ~1000-post windows beyond
// what the plain listing endpoints can surface.
//
// query can be a plain term, a phrase in quotes, or a Reddit search
// operator like `flair:"X"`, `author:alice`, or `self:yes`.
// timeFilter is one of "", "all", "year", "month", "week", "day", "hour".
func (s *Scraper) FetchSearch(subreddit, query, timeFilter string, seen map[string]bool) ([]Post, error) {
	return s.fetchSearchPages(subreddit, query, timeFilter, seen)
}

// fetchListingPages paginates a single sort/time listing endpoint,
// adding only posts not already in the seen set. Stops early after
// 3 consecutive pages with zero new posts to avoid wasting requests.
func (s *Scraper) fetchListingPages(subreddit, sortOrder, timeFilter string, seen map[string]bool) ([]Post, error) {
	var posts []Post
	after := ""
	count := 0
	staleRun := 0

	for {
		path := fmt.Sprintf("/r/%s/%s.json?limit=100&raw_json=1", subreddit, sortOrder)
		if timeFilter != "" {
			path += "&t=" + timeFilter
		}
		if after != "" {
			path += "&after=" + after + "&count=" + strconv.Itoa(count)
		}

		var listing redditListing
		if err := s.pool.getJSON(path, &listing); err != nil {
			return posts, err
		}

		if len(listing.Data.Children) == 0 {
			break
		}

		newCount := 0
		for _, child := range listing.Data.Children {
			if child.Kind != "t3" {
				continue
			}
			p := parsePost(child.Data)
			if p.ID == "" {
				continue
			}
			count++
			if !seen[p.ID] {
				seen[p.ID] = true
				posts = append(posts, p)
				newCount++
			}
		}

		if newCount == 0 {
			staleRun++
			if staleRun >= 3 {
				break
			}
		} else {
			staleRun = 0
		}

		s.emit(Progress{
			Phase:        "posts",
			PostsFetched: len(seen),
			Message:      fmt.Sprintf("fetched page (sort=%s, %d new, %d total unique)", sortOrder, newCount, len(seen)),
		})

		if s.opts.PostLimit > 0 && len(seen) >= s.opts.PostLimit {
			break
		}

		after = listing.Data.After
		if after == "" {
			break
		}
	}

	return posts, nil
}

// fetchSearchPages paginates the subreddit search endpoint. Each
// distinct query gets its own ~1000-post window, so cycling through
// queries (flair facets, keyword buckets, time windows) can multiply
// the total coverage far beyond what plain listings allow.
func (s *Scraper) fetchSearchPages(subreddit, query, timeFilter string, seen map[string]bool) ([]Post, error) {
	if seen == nil {
		seen = make(map[string]bool)
	}
	var posts []Post
	after := ""
	count := 0
	staleRun := 0

	for {
		path := fmt.Sprintf("/r/%s/search.json?q=%s&restrict_sr=on&sort=new&limit=100&raw_json=1",
			subreddit, urlQueryEscape(query))
		if timeFilter != "" {
			path += "&t=" + timeFilter
		}
		if after != "" {
			path += "&after=" + after + "&count=" + strconv.Itoa(count)
		}

		var listing redditListing
		if err := s.pool.getJSON(path, &listing); err != nil {
			return posts, err
		}

		if len(listing.Data.Children) == 0 {
			break
		}

		newCount := 0
		for _, child := range listing.Data.Children {
			if child.Kind != "t3" {
				continue
			}
			p := parsePost(child.Data)
			if p.ID == "" {
				continue
			}
			count++
			if !seen[p.ID] {
				seen[p.ID] = true
				posts = append(posts, p)
				newCount++
			}
		}

		if newCount == 0 {
			staleRun++
			if staleRun >= 3 {
				break
			}
		} else {
			staleRun = 0
		}

		s.emit(Progress{
			Phase:        "posts",
			PostsFetched: len(seen),
			Message:      fmt.Sprintf("search %q: +%d (%d total unique)", query, newCount, len(seen)),
		})

		if s.opts.PostLimit > 0 && len(seen) >= s.opts.PostLimit {
			break
		}

		after = listing.Data.After
		if after == "" {
			break
		}
	}

	return posts, nil
}

// FetchPostsScroll uses the browser infinite-scroll endpoint as an
// alternative strategy. Useful if the JSON listing API is restricted.
func (s *Scraper) FetchPostsScroll(subreddit string) ([]Post, error) {
	var posts []Post
	page := 0
	seen := make(map[string]bool)
	stalePages := 0

	nextPath := fmt.Sprintf("/svc/shreddit/community-more-posts/new/?t=&name=%s&feedLength=0", subreddit)

	for nextPath != "" {
		body, err := s.pool.getHTML(nextPath)
		if err != nil {
			return posts, fmt.Errorf("scroll page %d: %w", page, err)
		}

		content := string(body)
		pagePosts := parsePostsFromHTML(content)
		if len(pagePosts) == 0 {
			break
		}

		newCount := 0
		for _, p := range pagePosts {
			if !seen[p.ID] {
				seen[p.ID] = true
				posts = append(posts, p)
				newCount++
			}
		}

		if newCount == 0 {
			stalePages++
			if stalePages >= 3 {
				break
			}
		} else {
			stalePages = 0
		}

		s.emit(Progress{
			Phase:        "posts",
			PostsFetched: len(posts),
			Message:      fmt.Sprintf("scroll: %d posts (page %d, %d new)", len(posts), page+1, newCount),
		})

		if s.opts.PostLimit > 0 && len(posts) >= s.opts.PostLimit {
			posts = posts[:s.opts.PostLimit]
			break
		}

		nextPath = extractNextPageURL(content)
		page++
	}

	return posts, nil
}

// FetchComments fetches the full comment tree for a post.
// Also backfills the post's SelfText from the JSON response.
func (s *Scraper) FetchComments(subreddit, postID string) ([]Comment, string, error) {
	path := fmt.Sprintf("/r/%s/comments/%s.json?limit=500&depth=%d&raw_json=1&sort=top&threaded=true",
		subreddit, postID, s.opts.CommentDepth)

	var listings []redditListing
	if err := s.pool.getJSON(path, &listings); err != nil {
		return nil, "", fmt.Errorf("fetching comments for %s: %w", postID, err)
	}

	if len(listings) < 2 {
		return nil, "", nil
	}

	// First listing has the post data — extract selftext
	selfText := ""
	if len(listings[0].Data.Children) > 0 {
		var rp rawPost
		_ = json.Unmarshal(listings[0].Data.Children[0].Data, &rp)
		selfText = rp.Selftext
	}

	comments := s.parseCommentListing(listings[1])

	moreIDs := s.collectMoreIDs(listings[1])
	if len(moreIDs) > 0 {
		additional, err := s.fetchMoreComments(subreddit, postID, moreIDs)
		// Treat a partial expansion of "more" comments as a warning, not a
		// hard error. Reddit routinely buries long-tail comment chains
		// behind dozens of "more" tokens; a 20-60s time budget on a proxied
		// pool will often exhaust before we've walked the tree. Failing the
		// whole scrape over it is almost always the wrong call — we already
		// have the top-level comments that drive 95% of analysis value.
		// Mirror the tolerant behaviour FetchSinglePost has always had.
		if err != nil {
			s.emit(Progress{
				Phase:   "comments",
				Message: fmt.Sprintf("r/%s post %s: partial 'more' comments: %v", subreddit, postID, err),
			})
		}
		comments = append(comments, additional...)
	}

	return comments, selfText, nil
}

// FetchSinglePost fetches a single post by ID including full metadata and the
// complete comment tree. This is the method behind the -url CLI flag.
func (s *Scraper) FetchSinglePost(subreddit, postID string) (*Post, error) {
	path := fmt.Sprintf("/r/%s/comments/%s.json?limit=500&depth=%d&raw_json=1&sort=top&threaded=true",
		subreddit, postID, s.opts.CommentDepth)

	var listings []redditListing
	if err := s.pool.getJSON(path, &listings); err != nil {
		return nil, fmt.Errorf("fetching post %s: %w", postID, err)
	}

	if len(listings) < 1 || len(listings[0].Data.Children) == 0 {
		return nil, fmt.Errorf("post %s not found", postID)
	}

	post := parsePost(listings[0].Data.Children[0].Data)

	if len(listings) >= 2 {
		post.Comments = s.parseCommentListing(listings[1])

		moreIDs := s.collectMoreIDs(listings[1])
		if len(moreIDs) > 0 {
			additional, err := s.fetchMoreComments(subreddit, postID, moreIDs)
			if err != nil {
				s.emit(Progress{Phase: "comments", Message: fmt.Sprintf("partial 'more' comments: %v", err)})
			}
			post.Comments = append(post.Comments, additional...)
		}
	}

	return &post, nil
}

// ParsePostURL extracts the subreddit name and post ID from a Reddit URL.
// Accepts formats like:
//
//	https://www.reddit.com/r/whoop/comments/1sf4xjz/...
//	reddit.com/r/whoop/comments/1sf4xjz/
//	/r/whoop/comments/1sf4xjz
func ParsePostURL(rawURL string) (subreddit, postID string, err error) {
	re := regexp.MustCompile(`/r/([^/]+)/comments/([^/]+)`)
	m := re.FindStringSubmatch(rawURL)
	if len(m) < 3 {
		return "", "", fmt.Errorf("could not parse Reddit post URL: %s", rawURL)
	}
	return m[1], m[2], nil
}

// ScrapeAll fetches subreddit info, all posts, and (optionally) all comments.
// If Options.Token is set, it authenticates the session before scraping.
func (s *Scraper) ScrapeAll(subredditName string) (*ScrapeResult, error) {
	if s.opts.Token != "" && !s.client.IsAuthenticated() {
		for _, c := range s.pool.clients {
			c.WithToken(s.opts.Token)
		}
		s.emit(Progress{Phase: "auth", Message: "using provided session token"})
	}

	sub, err := s.FetchSubreddit(subredditName)
	if err != nil {
		return nil, err
	}

	posts, err := s.FetchPosts(subredditName)
	if err != nil {
		return nil, err
	}

	if !s.opts.SkipComments {
		if err := s.fetchAllComments(subredditName, posts); err != nil {
			return nil, err
		}
	}

	return &ScrapeResult{
		Subreddit:  *sub,
		Posts:      posts,
		ScrapedAt:  time.Now().UTC(),
		TotalPosts: len(posts),
	}, nil
}

// fetchAllComments populates posts[*].Comments either sequentially
// (CommentWorkers <= 1, abort on first error) or in parallel
// (CommentWorkers >= 2, tolerate per-post errors).
func (s *Scraper) fetchAllComments(subreddit string, posts []Post) error {
	workers := s.opts.CommentWorkers
	if workers <= 1 {
		return s.fetchAllCommentsSequential(subreddit, posts)
	}
	return s.fetchAllCommentsParallel(subreddit, posts, workers)
}

func (s *Scraper) fetchAllCommentsSequential(subreddit string, posts []Post) error {
	for i := range posts {
		s.emit(Progress{
			Phase:        "comments",
			PostsFetched: i + 1,
			TotalPosts:   len(posts),
			PostID:       posts[i].ID,
			Message:      fmt.Sprintf("fetching comments for post %d/%d (%s)", i+1, len(posts), posts[i].ID),
		})

		comments, selfText, err := s.FetchComments(subreddit, posts[i].ID)
		if err != nil {
			return fmt.Errorf("post %s: %w", posts[i].ID, err)
		}
		posts[i].Comments = comments
		if selfText != "" {
			posts[i].SelfText = selfText
		}
	}
	return nil
}

func (s *Scraper) fetchAllCommentsParallel(subreddit string, posts []Post, workers int) error {
	if workers > len(posts) {
		workers = len(posts)
	}
	if workers < 1 {
		return nil
	}

	jobs := make(chan int, len(posts))
	var wg sync.WaitGroup
	var completed, failed int64

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				comments, selfText, err := s.FetchComments(subreddit, posts[i].ID)
				if err != nil {
					atomic.AddInt64(&failed, 1)
					s.emit(Progress{
						Phase:   "comments",
						PostID:  posts[i].ID,
						Message: fmt.Sprintf("post %s comments failed (continuing): %v", posts[i].ID, err),
					})
				} else {
					// Each goroutine writes to a unique slice index, so
					// no mutex is needed around posts[i] mutation.
					posts[i].Comments = comments
					if selfText != "" {
						posts[i].SelfText = selfText
					}
				}

				done := atomic.AddInt64(&completed, 1)
				fail := atomic.LoadInt64(&failed)
				s.emit(Progress{
					Phase:        "comments",
					PostsFetched: int(done),
					TotalPosts:   len(posts),
					PostID:       posts[i].ID,
					Message: fmt.Sprintf(
						"fetched comments for %d/%d posts (%d failures, %d workers)",
						done, len(posts), fail, workers),
				})
			}
		}()
	}

	for i := range posts {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return nil
}

func (s *Scraper) emit(p Progress) {
	if s.progress != nil {
		select {
		case s.progress <- p:
		default:
		}
	}
}

// --- scroll HTML parsing ---

var (
	shredditPostRe = regexp.MustCompile(`<shreddit-post\b[^>]*>`)
	nextPageRe     = regexp.MustCompile(`<faceplate-partial[^>]*src="(/svc/shreddit/community-more-posts/[^"]*)"`)
	attrRe         = regexp.MustCompile(`([\w-]+)="([^"]*)"`)
)

func parsePostsFromHTML(body string) []Post {
	var posts []Post
	for _, tag := range shredditPostRe.FindAllString(body, -1) {
		attrs := make(map[string]string)
		for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
			attrs[m[1]] = m[2]
		}

		id := strings.TrimPrefix(attrs["id"], "t3_")
		if id == "" {
			continue
		}

		score, _ := strconv.Atoi(attrs["score"])
		numComments, _ := strconv.Atoi(attrs["comment-count"])

		var createdUTC time.Time
		if ts := attrs["created-timestamp"]; ts != "" {
			createdUTC, _ = time.Parse("2006-01-02T15:04:05.000000+0000", ts)
			if createdUTC.IsZero() {
				createdUTC, _ = time.Parse(time.RFC3339, ts)
			}
		}

		_, isNSFW := attrs["nsfw"]
		if !isNSFW {
			// nsfw might be a boolean attr without a value, check original tag
			isNSFW = strings.Contains(tag, " nsfw")
		}

		posts = append(posts, Post{
			ID:          id,
			Subreddit:   attrs["subreddit-name"],
			Title:       html.UnescapeString(attrs["post-title"]),
			Author:      attrs["author"],
			Score:       score,
			NumComments: numComments,
			CreatedUTC:  createdUTC,
			Permalink:   attrs["permalink"],
			URL:         attrs["content-href"],
			Domain:      attrs["domain"],
			IsSelf:      attrs["post-type"] == "text",
			IsVideo:     attrs["post-type"] == "video",
			Over18:      isNSFW,
		})
	}
	return posts
}

// extractNextPageURL pulls the full next-page URL from the faceplate-partial
// element that Reddit embeds at the bottom of each scroll page.
func extractNextPageURL(body string) string {
	m := nextPageRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return html.UnescapeString(m[1])
}

// --- raw Reddit JSON structures ---

type redditListing struct {
	Kind string `json:"kind"`
	Data struct {
		After    string        `json:"after"`
		Before   string        `json:"before"`
		Children []redditChild `json:"children"`
	} `json:"data"`
}

type redditChild struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type rawPost struct {
	ID            string  `json:"id"`
	Subreddit     string  `json:"subreddit"`
	Title         string  `json:"title"`
	Author        string  `json:"author"`
	Selftext      string  `json:"selftext"`
	Score         int     `json:"score"`
	UpvoteRatio   float64 `json:"upvote_ratio"`
	NumComments   int     `json:"num_comments"`
	CreatedUTC    float64 `json:"created_utc"`
	Permalink     string  `json:"permalink"`
	URL           string  `json:"url"`
	Domain        string  `json:"domain"`
	IsSelf        bool    `json:"is_self"`
	IsVideo       bool    `json:"is_video"`
	Over18        bool    `json:"over_18"`
	Stickied      bool    `json:"stickied"`
	Locked        bool    `json:"locked"`
	Archived      bool    `json:"archived"`
	Distinguished string  `json:"distinguished"`
	LinkFlairText string  `json:"link_flair_text"`
}

type rawComment struct {
	ID               string          `json:"id"`
	Author           string          `json:"author"`
	Body             string          `json:"body"`
	Score            int             `json:"score"`
	CreatedUTC       float64         `json:"created_utc"`
	ParentID         string          `json:"parent_id"`
	Permalink        string          `json:"permalink"`
	Depth            int             `json:"depth"`
	IsSubmitter      bool            `json:"is_submitter"`
	Stickied         bool            `json:"stickied"`
	Distinguished    string          `json:"distinguished"`
	Controversiality int             `json:"controversiality"`
	Edited           json.RawMessage `json:"edited"`
	Replies          json.RawMessage `json:"replies"`
}

type rawMore struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	ParentID string   `json:"parent_id"`
	Count    int      `json:"count"`
	Children []string `json:"children"`
}

func parsePost(raw json.RawMessage) Post {
	var rp rawPost
	_ = json.Unmarshal(raw, &rp)
	return Post{
		ID:            rp.ID,
		Subreddit:     rp.Subreddit,
		Title:         rp.Title,
		Author:        rp.Author,
		SelfText:      rp.Selftext,
		Score:         rp.Score,
		UpvoteRatio:   rp.UpvoteRatio,
		NumComments:   rp.NumComments,
		CreatedUTC:    time.Unix(int64(rp.CreatedUTC), 0),
		Permalink:     rp.Permalink,
		URL:           rp.URL,
		Domain:        rp.Domain,
		IsSelf:        rp.IsSelf,
		IsVideo:       rp.IsVideo,
		Over18:        rp.Over18,
		Stickied:      rp.Stickied,
		Locked:        rp.Locked,
		Archived:      rp.Archived,
		Distinguished: rp.Distinguished,
		LinkFlairText: rp.LinkFlairText,
	}
}

func parseComment(raw json.RawMessage) (Comment, []Comment) {
	var rc rawComment
	_ = json.Unmarshal(raw, &rc)

	edited := false
	if len(rc.Edited) > 0 && string(rc.Edited) != "false" && string(rc.Edited) != "null" {
		edited = true
	}

	c := Comment{
		ID:               rc.ID,
		Author:           rc.Author,
		Body:             rc.Body,
		Score:            rc.Score,
		CreatedUTC:       time.Unix(int64(rc.CreatedUTC), 0),
		ParentID:         rc.ParentID,
		Permalink:        rc.Permalink,
		Depth:            rc.Depth,
		IsSubmitter:      rc.IsSubmitter,
		Stickied:         rc.Stickied,
		Distinguished:    rc.Distinguished,
		Controversiality: rc.Controversiality,
		Edited:           edited,
	}

	var childComments []Comment
	if len(rc.Replies) > 0 && string(rc.Replies) != `""` && string(rc.Replies) != "null" {
		var replyListing redditListing
		if err := json.Unmarshal(rc.Replies, &replyListing); err == nil {
			for _, child := range replyListing.Data.Children {
				if child.Kind == "t1" {
					reply, nested := parseComment(child.Data)
					c.Replies = append(c.Replies, reply)
					childComments = append(childComments, nested...)
				}
			}
		}
	}

	return c, childComments
}

func (s *Scraper) parseCommentListing(listing redditListing) []Comment {
	var comments []Comment
	for _, child := range listing.Data.Children {
		if child.Kind == "t1" {
			c, _ := parseComment(child.Data)
			comments = append(comments, c)
		}
	}
	return comments
}

func (s *Scraper) collectMoreIDs(listing redditListing) []string {
	var ids []string
	for _, child := range listing.Data.Children {
		if child.Kind == "more" {
			var m rawMore
			if err := json.Unmarshal(child.Data, &m); err == nil {
				ids = append(ids, m.Children...)
			}
		}
	}
	return ids
}

func (s *Scraper) fetchMoreComments(subreddit, postID string, ids []string) ([]Comment, error) {
	var comments []Comment
	consecutiveErr := 0
	budget := s.opts.MoreCommentsTimeout
	if budget <= 0 {
		budget = defaultMoreCommentsTimeout
	}
	deadline := time.Now().Add(budget)

	for _, commentID := range ids {
		if time.Now().After(deadline) {
			return comments, fmt.Errorf("more-comments: time budget exceeded (%d/%d expanded)", len(comments), len(ids))
		}

		path := fmt.Sprintf("/r/%s/comments/%s/_/%s.json?raw_json=1&depth=%d",
			subreddit, postID, commentID, s.opts.CommentDepth)

		var listings []redditListing
		if err := s.pool.getJSON(path, &listings); err != nil {
			consecutiveErr++
			if consecutiveErr >= 3 {
				return comments, fmt.Errorf("more-comments: %d consecutive errors", consecutiveErr)
			}
			continue
		}
		consecutiveErr = 0
		if len(listings) >= 2 {
			for _, child := range listings[1].Data.Children {
				if child.Kind == "t1" {
					c, _ := parseComment(child.Data)
					comments = append(comments, c)
				}
			}
		}
	}

	return comments, nil
}
