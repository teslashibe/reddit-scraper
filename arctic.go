package redditscraper

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Arctic Shift is a community-maintained Reddit historical archive
// that effectively replaces the dead Pushshift API. It exposes a
// simple REST endpoint at https://arctic-shift.photon-reddit.com
// returning Pushshift-format post JSON identical to Reddit's own
// `data` blob, so we can decode it with the same `rawPost` struct.
//
// Empirically (see cmd/depth-test): on r/Peptides Arctic Shift
// returned 67,144 posts going back to 2014, vs ~2,000 from the live
// Reddit API. ~10s per 1000 posts, ~9 minutes for the full 67k.

const (
	defaultArcticAPIBase   = "https://arctic-shift.photon-reddit.com"
	defaultArcticPageSize  = 100
	defaultArcticGap       = 200 * time.Millisecond
	arcticConsecutiveErrCap = 5
	arcticHTTPTimeout      = 30 * time.Second
)

type arcticResponse struct {
	Data  []json.RawMessage `json:"data"`
	Error string            `json:"error,omitempty"`
}

// fetchPostsArctic walks the Arctic Shift archive newest-first via
// `before=<unix_ts>` cursor pagination. Stops when:
//   - Options.PostLimit reached
//   - Options.OldestPost cutoff reached (passed as `after=` for
//     server-side filtering)
//   - empty page (full available history exhausted)
//
// Returns the same []Post shape the Reddit backend produces.
func (s *Scraper) fetchPostsArctic(subreddit string) ([]Post, error) {
	base := s.opts.ArcticAPIBase
	if base == "" {
		base = defaultArcticAPIBase
	}
	gap := s.opts.ArcticRequestGap
	if gap <= 0 {
		gap = defaultArcticGap
	}

	httpClient := &http.Client{Timeout: arcticHTTPTimeout}

	var posts []Post
	seen := make(map[string]bool)
	before := "" // unix-second cursor; "" means start at newest
	page := 0
	consecutiveErr := 0
	lastReq := time.Time{}

	for {
		// polite gap
		if elapsed := time.Since(lastReq); elapsed < gap {
			time.Sleep(gap - elapsed)
		}
		lastReq = time.Now()

		page++
		path := fmt.Sprintf("%s/api/posts/search?subreddit=%s&limit=%d&sort=desc",
			base, subreddit, defaultArcticPageSize)
		if before != "" {
			path += "&before=" + before
		}
		if !s.opts.OldestPost.IsZero() {
			path += "&after=" + strconv.FormatInt(s.opts.OldestPost.Unix(), 10)
		}

		req, err := http.NewRequest("GET", path, nil)
		if err != nil {
			return posts, fmt.Errorf("arctic-shift: %w", err)
		}
		req.Header.Set("User-Agent", s.userAgent())
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			consecutiveErr++
			s.emit(Progress{
				Phase:   "posts",
				Message: fmt.Sprintf("arctic-shift transport error (attempt %d/%d): %v", consecutiveErr, arcticConsecutiveErrCap, err),
			})
			if consecutiveErr >= arcticConsecutiveErrCap {
				if len(posts) > 0 {
					// partial success — return what we have
					return posts, nil
				}
				return posts, fmt.Errorf("arctic-shift: %d consecutive errors: %w", consecutiveErr, err)
			}
			time.Sleep(time.Duration(consecutiveErr) * time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			consecutiveErr++
			s.emit(Progress{
				Phase:   "posts",
				Message: fmt.Sprintf("arctic-shift status %d (attempt %d/%d)", resp.StatusCode, consecutiveErr, arcticConsecutiveErrCap),
			})
			if consecutiveErr >= arcticConsecutiveErrCap {
				if len(posts) > 0 {
					return posts, nil
				}
				return posts, fmt.Errorf("arctic-shift: status %d: %s", resp.StatusCode, truncateStr(string(body), 200))
			}
			time.Sleep(time.Duration(consecutiveErr) * time.Second)
			continue
		}

		var ar arcticResponse
		if err := json.Unmarshal(body, &ar); err != nil {
			return posts, fmt.Errorf("arctic-shift: bad json: %w (body preview: %s)", err, truncateStr(string(body), 200))
		}
		if ar.Error != "" {
			return posts, fmt.Errorf("arctic-shift: api error: %s", ar.Error)
		}
		if len(ar.Data) == 0 {
			break
		}
		consecutiveErr = 0

		var oldestTS int64
		newCount := 0
		for _, raw := range ar.Data {
			p := parsePost(raw)
			if p.ID == "" {
				continue
			}
			if !seen[p.ID] {
				seen[p.ID] = true
				posts = append(posts, p)
				newCount++
			}
			ts := p.CreatedUTC.Unix()
			if oldestTS == 0 || ts < oldestTS {
				oldestTS = ts
			}
		}

		s.emit(Progress{
			Phase:        "posts",
			PostsFetched: len(posts),
			Message:      fmt.Sprintf("arctic-shift page %d: +%d (%d total unique)", page, newCount, len(posts)),
		})

		if s.opts.PostLimit > 0 && len(posts) >= s.opts.PostLimit {
			posts = posts[:s.opts.PostLimit]
			break
		}

		if newCount == 0 {
			// Same page returned twice — Arctic Shift cursor stuck.
			break
		}

		// Advance cursor to one second before the oldest post in this
		// page so we don't redownload the boundary post.
		before = strconv.FormatInt(oldestTS, 10)
	}

	return posts, nil
}

// userAgent returns the configured UA or the package default.
func (s *Scraper) userAgent() string {
	if s.opts.UserAgent != "" {
		return s.opts.UserAgent
	}
	return defaultUserAgent
}
