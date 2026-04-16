// Diagnostic tool: measure how deep each scraping strategy can reach
// against a subreddit. Reports per-strategy count, oldest date, and
// whether the 1000/listing cap was hit. Also probes additional
// endpoints that might break past it (extra sort/time combos,
// search-based pagination, flair faceting).
//
// Usage:
//
//	go run ./cmd/depth-test -sub Peptides -proxies "Webshare 100 proxies.txt"
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	redditscraper "github.com/teslashibe/reddit-scraper"
)

type stratResult struct {
	name      string
	postCount int
	newAdded  int // posts not already seen by a previous strategy
	oldest    time.Time
	newest    time.Time
	err       string
}

func (r stratResult) log(cum int) {
	oldest := ""
	if !r.oldest.IsZero() {
		oldest = r.oldest.Format("2006-01-02")
	}
	errMsg := ""
	if r.err != "" {
		errMsg = " ERR=" + r.err
	}
	log.Printf("  %-38s posts=%4d new=%4d oldest=%s cum=%d%s",
		r.name, r.postCount, r.newAdded, oldest, cum, errMsg)
}

type probe struct {
	name       string
	fetch      func(scraper *redditscraper.Scraper, seen map[string]bool) ([]redditscraper.Post, error)
	skipOnSeen bool
}

func listingProbe(name, subreddit, sortOrder, t string) probe {
	return probe{
		name: name,
		fetch: func(s *redditscraper.Scraper, seen map[string]bool) ([]redditscraper.Post, error) {
			return s.FetchListingPages(subreddit, sortOrder, t, seen)
		},
	}
}

func main() {
	sub := flag.String("sub", "Peptides", "Subreddit to test")
	proxyFile := flag.String("proxies", "", "Path to proxy file")
	gap := flag.Int("gap", 200, "ms between requests")
	skipFlair := flag.Bool("skipflair", false, "Skip per-flair probe")
	skipSearch := flag.Bool("skipsearch", false, "Skip search probe")
	flag.Parse()

	subreddit := strings.TrimPrefix(*sub, "r/")

	var proxies []string
	if *proxyFile != "" {
		data, err := os.ReadFile(*proxyFile)
		if err == nil {
			for _, ln := range strings.Split(string(data), "\n") {
				ln = strings.TrimSpace(ln)
				if ln == "" || strings.HasPrefix(ln, "#") {
					continue
				}
				parts := strings.SplitN(ln, ":", 4)
				if len(parts) == 4 {
					proxies = append(proxies, fmt.Sprintf("http://%s:%s@%s:%s",
						parts[2], parts[3], parts[0], parts[1]))
				}
			}
		}
		log.Printf("loaded %d proxies from %s", len(proxies), *proxyFile)
	}

	opts := &redditscraper.Options{
		MinRequestGap: time.Duration(*gap) * time.Millisecond,
		ProxyURLs:     proxies,
	}
	progress := make(chan redditscraper.Progress, 100)
	go func() {
		for p := range progress {
			_ = p
		}
	}()
	scraper := redditscraper.New(opts, progress)

	if scraper.ProxyCount() > 1 {
		log.Printf("health-checking %d proxies...", scraper.ProxyCount()-1)
		h, d := scraper.HealthCheck(subreddit)
		log.Printf("pool: %d healthy, %d dead", h, d)
	}

	log.Printf("━━━ depth-test on r/%s ━━━", subreddit)

	// Build probe list
	var probes []probe

	// Phase A: strategies the current scraper already uses
	phaseA := []probe{
		listingProbe("A:new", subreddit, "new", ""),
		listingProbe("A:top/all", subreddit, "top", "all"),
		listingProbe("A:top/year", subreddit, "top", "year"),
		listingProbe("A:top/month", subreddit, "top", "month"),
		listingProbe("A:controversial/all", subreddit, "controversial", "all"),
		listingProbe("A:controversial/year", subreddit, "controversial", "year"),
		listingProbe("A:hot", subreddit, "hot", ""),
	}

	// Phase B: additional sort/time combos
	phaseB := []probe{
		listingProbe("B:top/week", subreddit, "top", "week"),
		listingProbe("B:top/day", subreddit, "top", "day"),
		listingProbe("B:controversial/month", subreddit, "controversial", "month"),
		listingProbe("B:controversial/week", subreddit, "controversial", "week"),
		listingProbe("B:rising", subreddit, "rising", ""),
	}

	probes = append(probes, phaseA...)
	probes = append(probes, phaseB...)

	// Phase C: search-based pagination (each query is independent)
	if !*skipSearch {
		for _, q := range []string{"a", "the", "i", "is", "and", "for", "to", "of"} {
			q := q
			probes = append(probes, probe{
				name: fmt.Sprintf("C:search q=%q", q),
				fetch: func(s *redditscraper.Scraper, seen map[string]bool) ([]redditscraper.Post, error) {
					return s.FetchSearch(subreddit, q, "", seen)
				},
			})
		}
	}

	// Inject subreddit into each probe
	seen := make(map[string]bool)
	allPosts := make(map[string]redditscraper.Post)
	var results []stratResult

	log.Printf("\n--- running %d probes ---", len(probes))
	for _, p := range probes {
		start := time.Now()
		wrapped := p.fetch
		posts, err := func() ([]redditscraper.Post, error) {
			return wrapped(scraper, seen)
		}()
		_ = start

		res := stratResult{name: p.name}
		if err != nil {
			res.err = err.Error()
			if len(res.err) > 80 {
				res.err = res.err[:80] + "..."
			}
		}
		for _, post := range posts {
			res.postCount++
			res.newAdded++ // already dedupe'd by the seen map
			if _, ok := allPosts[post.ID]; !ok {
				allPosts[post.ID] = post
			}
			if res.oldest.IsZero() || post.CreatedUTC.Before(res.oldest) {
				res.oldest = post.CreatedUTC
			}
			if post.CreatedUTC.After(res.newest) {
				res.newest = post.CreatedUTC
			}
		}
		results = append(results, res)
		res.log(len(allPosts))
	}

	// Phase D: flair-facet search. Flairs act as hard facets — each
	// returns its own ~1000-post window. Only run if search is enabled.
	if !*skipFlair && !*skipSearch {
		log.Printf("\n--- Phase D: flair-facet search ---")
		flairs := discoverFlairs(allPosts)
		log.Printf("discovered %d flairs", len(flairs))
		for _, fl := range flairs {
			q := fmt.Sprintf(`flair:"%s"`, fl)
			posts, err := scraper.FetchSearch(subreddit, q, "", seen)
			res := stratResult{name: fmt.Sprintf("D:flair=%s", fl)}
			if err != nil {
				res.err = err.Error()
			}
			for _, post := range posts {
				res.postCount++
				if _, ok := allPosts[post.ID]; !ok {
					allPosts[post.ID] = post
					res.newAdded++
				}
				if res.oldest.IsZero() || post.CreatedUTC.Before(res.oldest) {
					res.oldest = post.CreatedUTC
				}
				if post.CreatedUTC.After(res.newest) {
					res.newest = post.CreatedUTC
				}
			}
			results = append(results, res)
			res.log(len(allPosts))
		}
	}

	// Phase E: the shreddit scroll endpoint
	log.Printf("\n--- Phase E: shreddit scroll endpoint ---")
	scrollPosts, err := scraper.FetchPostsScroll(subreddit)
	res := stratResult{name: "E:scroll"}
	if err != nil {
		res.err = err.Error()
	}
	for _, post := range scrollPosts {
		res.postCount++
		if _, ok := allPosts[post.ID]; !ok {
			allPosts[post.ID] = post
			res.newAdded++
		}
		if res.oldest.IsZero() || post.CreatedUTC.Before(res.oldest) {
			res.oldest = post.CreatedUTC
		}
		if post.CreatedUTC.After(res.newest) {
			res.newest = post.CreatedUTC
		}
	}
	results = append(results, res)
	res.log(len(allPosts))

	close(progress)

	// ======== FINAL REPORT ========
	log.Printf("\n━━━ RESULTS (sorted by contribution) ━━━")
	sort.Slice(results, func(i, j int) bool { return results[i].newAdded > results[j].newAdded })

	log.Printf("%-40s %6s %6s %-10s %-10s %s",
		"strategy", "posts", "new", "oldest", "newest", "error")
	for _, r := range results {
		oldest := ""
		if !r.oldest.IsZero() {
			oldest = r.oldest.Format("2006-01-02")
		}
		newest := ""
		if !r.newest.IsZero() {
			newest = r.newest.Format("2006-01-02")
		}
		log.Printf("%-40s %6d %6d %-10s %-10s %s",
			r.name, r.postCount, r.newAdded, oldest, newest, r.err)
	}

	log.Printf("\n━━━ TOTAL UNIQUE POSTS: %d ━━━", len(allPosts))
	if len(allPosts) > 0 {
		var oldest, newest time.Time
		byYear := map[int]int{}
		for _, p := range allPosts {
			if oldest.IsZero() || p.CreatedUTC.Before(oldest) {
				oldest = p.CreatedUTC
			}
			if p.CreatedUTC.After(newest) {
				newest = p.CreatedUTC
			}
			byYear[p.CreatedUTC.Year()]++
		}
		log.Printf("Date range: %s → %s (%.1f years)",
			oldest.Format("2006-01-02"), newest.Format("2006-01-02"),
			newest.Sub(oldest).Hours()/24/365)

		var years []int
		for y := range byYear {
			years = append(years, y)
		}
		sort.Ints(years)
		log.Printf("Distribution by year:")
		for _, y := range years {
			bar := strings.Repeat("█", min(40, byYear[y]/5))
			log.Printf("  %d: %4d  %s", y, byYear[y], bar)
		}

		// Phase contributions
		log.Printf("\nPhase contributions (unique new posts):")
		byPhase := map[string]int{}
		for _, r := range results {
			if len(r.name) >= 2 {
				byPhase[r.name[:1]] += r.newAdded
			}
		}
		for _, ph := range []string{"A", "B", "C", "D", "E"} {
			if byPhase[ph] > 0 {
				log.Printf("  Phase %s: %d unique posts", ph, byPhase[ph])
			}
		}
	}
}

func discoverFlairs(posts map[string]redditscraper.Post) []string {
	set := map[string]bool{}
	for _, p := range posts {
		if p.LinkFlairText != "" {
			set[p.LinkFlairText] = true
		}
	}
	var out []string
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
