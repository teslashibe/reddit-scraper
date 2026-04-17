package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	redditscraper "github.com/teslashibe/reddit-scraper"
)

func main() {
	sub := flag.String("sub", "", "Subreddit to scrape (e.g. whoop, Peptides)")
	postURL := flag.String("url", "", "Single Reddit post URL to scrape (e.g. https://www.reddit.com/r/whoop/comments/abc123/...)")
	outDir := flag.String("out", ".", "Output directory")
	gap := flag.Int("gap", 1000, "Minimum ms between requests")
	token := flag.String("token", "", "Reddit token_v2 cookie (for NSFW/gated subs)")
	fresh := flag.Bool("fresh", false, "Ignore existing progress and start from scratch")
	proxies := flag.String("proxies", "", "Comma-separated proxy URLs (http://host:port or socks5://host:port), or path to a file with one proxy per line")
	source := flag.String("source", "", "Post discovery backend: '' (auto, default), 'arctic' (Arctic Shift archive — 10-30× more posts), or 'reddit' (live listing API only)")
	since := flag.String("since", "", "Only fetch posts created on or after this date (YYYY-MM-DD). Server-side filter when source=arctic.")
	maxPosts := flag.Int("max", 0, "Cap total posts collected (0 = unlimited)")
	skipComments := flag.Bool("skip-comments", false, "Skip comment fetching (much faster for large historical scrapes)")
	workers := flag.Int("workers", 0, "Comment-fetch concurrency (0 = auto: roughly one worker per healthy proxy, capped at 32)")
	flag.Parse()

	postSource, err := parsePostSource(*source)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	oldest, err := parseSince(*since)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *postURL != "" {
		scrapeSinglePost(*postURL, *outDir, *gap, *token, *proxies)
		return
	}

	if *sub == "" {
		if flag.NArg() > 0 {
			*sub = flag.Arg(0)
		} else {
			fmt.Fprintf(os.Stderr, "Usage: scrape -sub <subreddit> [-out dir] [-gap ms] [-proxies urls] [-token cookie] [-fresh]\n       scrape -url <reddit-post-url> [-out dir] [-gap ms] [-token cookie]\n       scrape <subreddit>\n")
			os.Exit(1)
		}
	}
	subreddit := strings.TrimPrefix(*sub, "r/")

	os.MkdirAll(*outDir, 0o755)
	outFile := filepath.Join(*outDir, fmt.Sprintf("r_%s_%s.jsonl", subreddit, time.Now().Format("2006-01-02")))
	manifestFile := outFile + ".manifest.json"

	log.Printf("Scraping r/%s — posts + full comment trees", subreddit)
	log.Printf("Output: %s | gap: %dms", outFile, *gap)

	progress := make(chan redditscraper.Progress, 500)
	go func() {
		for p := range progress {
			log.Printf("[%s] %s", p.Phase, p.Message)
		}
	}()

	opts := &redditscraper.Options{
		MinRequestGap: time.Duration(*gap) * time.Millisecond,
		Source:        postSource,
		OldestPost:    oldest,
		PostLimit:     *maxPosts,
		SkipComments:  *skipComments,
	}
	if *token != "" {
		opts.Token = *token
	}
	if *proxies != "" {
		opts.ProxyURLs = parseProxies(*proxies)
	}
	scraper := redditscraper.New(opts, progress)
	log.Printf("Source: %s | OldestPost: %s | MaxPosts: %d | SkipComments: %v",
		describeSource(postSource), describeSince(oldest), *maxPosts, *skipComments)
	if scraper.ProxyCount() > 1 {
		log.Printf("Testing %d proxies...", scraper.ProxyCount()-1)
		healthy, dead := scraper.HealthCheck(subreddit)
		log.Printf("Pool: %d healthy clients, %d dead proxies removed", healthy, dead)
	}

	// Phase 1: Get post list (from manifest or fresh fetch)
	var posts []redditscraper.Post
	if !*fresh {
		posts = loadManifest(manifestFile)
	}

	if len(posts) > 0 {
		log.Printf("Phase 1: Loaded %d posts from manifest (skipping fetch)", len(posts))
	} else {
		log.Println("Phase 1: Fetching all posts...")
		var err error
		posts, err = scraper.FetchPosts(subreddit)
		if err != nil {
			log.Fatalf("FetchPosts failed: %v", err)
		}
		log.Printf("Got %d unique posts. Saving manifest...", len(posts))
		saveManifest(manifestFile, posts)
	}

	// Posts-only mode: skip Phase 2 entirely and emit just metadata.
	// Useful for huge historical scrapes where comment fetching would
	// take days. Each line is still a valid Post JSON with empty
	// Comments — downstream consumers don't need to special-case it.
	if *skipComments {
		writeStart := time.Now()
		f, err := os.Create(outFile)
		if err != nil {
			log.Fatalf("Open output file: %v", err)
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		for i := range posts {
			if err := enc.Encode(posts[i]); err != nil {
				log.Fatalf("Write post %s: %v", posts[i].ID, err)
			}
		}
		stat, _ := f.Stat()
		log.Printf("━━━ Done (posts-only) — wrote in %s ━━━", time.Since(writeStart).Round(time.Millisecond))
		log.Printf("Output: %s (%.1f MB)", outFile, float64(stat.Size())/1024/1024)
		log.Printf("Total: %d posts (comments skipped)", len(posts))
		os.Remove(manifestFile)
		close(progress)
		return
	}

	// Phase 2: Fetch comments, resuming past completed posts
	done := make(map[string]bool)
	if !*fresh {
		done = scanCompleted(outFile)
	}
	if len(done) > 0 {
		log.Printf("Phase 2: Resuming — %d/%d posts already completed", len(done), len(posts))
	} else {
		log.Println("Phase 2: Fetching comments...")
	}

	f, err := os.OpenFile(outFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatalf("Open output file: %v", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	skipped := 0

	// Skip already-completed posts first so rate/ETA only reflect real work
	remaining := make([]int, 0, len(posts)-len(done))
	for i := range posts {
		if done[posts[i].ID] {
			skipped++
		} else {
			remaining = append(remaining, i)
		}
	}

	todo := len(remaining)

	workerCount := *workers
	if workerCount <= 0 {
		// Auto: roughly one worker per healthy proxy, capped at 32 to
		// avoid OS file-descriptor / goroutine pressure on huge pools.
		// scraper.ProxyCount() includes the direct client, so subtract
		// one to size against actual proxies.
		workerCount = scraper.ProxyCount()
		if workerCount > 32 {
			workerCount = 32
		}
		if workerCount < 1 {
			workerCount = 1
		}
	}
	if workerCount > todo && todo > 0 {
		workerCount = todo
	}
	log.Printf("Phase 2: %d to fetch, %d already done, %d total (workers=%d)", todo, skipped, len(posts), workerCount)

	start := time.Now()

	// Fan-out comment fetches across workerCount goroutines that share
	// the proxy pool. Each goroutine writes to its own posts[idx]
	// (unique index → no race) but file encoding + counter updates +
	// progress logging are serialized via a single mutex so JSONL stays
	// one-post-per-line atomic and resumable.
	var (
		fileMu        sync.Mutex
		written       int64
		errored       int64
		totalComments int64
	)

	jobs := make(chan int, todo)
	var wg sync.WaitGroup
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				postStart := time.Now()
				comments, selfText, fetchErr := scraper.FetchComments(subreddit, posts[idx].ID)
				if fetchErr != nil {
					atomic.AddInt64(&errored, 1)
				}
				posts[idx].Comments = comments
				if selfText != "" {
					posts[idx].SelfText = selfText
				}
				nc := countAll(comments)
				atomic.AddInt64(&totalComments, int64(nc))

				fileMu.Lock()
				if err := enc.Encode(posts[idx]); err != nil {
					fileMu.Unlock()
					log.Fatalf("Write post %s: %v", posts[idx].ID, err)
				}
				wIdx := atomic.AddInt64(&written, 1)
				eIdx := atomic.LoadInt64(&errored)
				fileMu.Unlock()

				postDur := time.Since(postStart).Round(time.Millisecond)
				elapsed := time.Since(start)
				rate := float64(wIdx) / elapsed.Seconds()
				left := int64(todo) - wIdx
				eta := time.Duration(0)
				if rate > 0 {
					eta = time.Duration(float64(left)/rate) * time.Second
				}
				status := "ok"
				if fetchErr != nil {
					status = "ERR"
				}
				log.Printf("  #%d/%d  post=%s  comments=%d  %s  %s  [%.1f p/s · ETA %s · %d err]",
					wIdx, todo, posts[idx].ID, nc, status, postDur, rate, eta.Round(time.Second), eIdx)
			}
		}()
	}
	for _, idx := range remaining {
		jobs <- idx
	}
	close(jobs)
	wg.Wait()

	writtenFinal := int(atomic.LoadInt64(&written))
	erroredFinal := int(atomic.LoadInt64(&errored))
	totalCommentsFinal := int(atomic.LoadInt64(&totalComments))
	close(progress)

	stat, _ := f.Stat()
	sizeMB := float64(stat.Size()) / 1024 / 1024

	log.Printf("━━━ Done in %s ━━━", time.Since(start).Round(time.Second))
	log.Printf("Output: %s (%.1f MB)", outFile, sizeMB)
	log.Printf("Total: %d posts (%d new, %d resumed, %d errors), %d comments",
		len(posts), writtenFinal, skipped, erroredFinal, totalCommentsFinal)

	if writtenFinal > 0 && skipped+writtenFinal == len(posts) {
		os.Remove(manifestFile)
		log.Printf("Manifest removed (scrape complete)")
	}
}

func loadManifest(path string) []redditscraper.Post {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var posts []redditscraper.Post
	if err := json.Unmarshal(data, &posts); err != nil {
		return nil
	}
	return posts
}

func saveManifest(path string, posts []redditscraper.Post) {
	stripped := make([]redditscraper.Post, len(posts))
	for i, p := range posts {
		stripped[i] = redditscraper.Post{
			ID:         p.ID,
			Subreddit:  p.Subreddit,
			Title:      p.Title,
			Permalink:  p.Permalink,
			CreatedUTC: p.CreatedUTC,
			IsSelf:     p.IsSelf,
		}
	}
	data, err := json.Marshal(stripped)
	if err != nil {
		log.Printf("warning: could not save manifest: %v", err)
		return
	}
	os.WriteFile(path, data, 0o644)
}

func scanCompleted(path string) map[string]bool {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	done := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 20*1024*1024)
	for scanner.Scan() {
		var stub struct{ ID string `json:"id"` }
		if json.Unmarshal(scanner.Bytes(), &stub) == nil && stub.ID != "" {
			done[stub.ID] = true
		}
	}
	return done
}

func countAll(comments []redditscraper.Comment) int {
	n := len(comments)
	for _, c := range comments {
		n += countAll(c.Replies)
	}
	return n
}

func parseProxies(input string) []string {
	var lines []string
	if data, err := os.ReadFile(input); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				lines = append(lines, line)
			}
		}
	} else {
		for _, u := range strings.Split(input, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				lines = append(lines, u)
			}
		}
	}

	var urls []string
	for _, line := range lines {
		urls = append(urls, toProxyURL(line))
	}
	return urls
}

func scrapeSinglePost(rawURL, outDir string, gap int, token, proxies string) {
	subreddit, postID, err := redditscraper.ParsePostURL(rawURL)
	if err != nil {
		log.Fatalf("Invalid URL: %v", err)
	}

	os.MkdirAll(outDir, 0o755)
	outFile := filepath.Join(outDir, fmt.Sprintf("r_%s_post_%s.jsonl", subreddit, postID))

	log.Printf("Scraping single post: r/%s/comments/%s", subreddit, postID)

	progress := make(chan redditscraper.Progress, 100)
	go func() {
		for p := range progress {
			log.Printf("[%s] %s", p.Phase, p.Message)
		}
	}()

	opts := &redditscraper.Options{
		MinRequestGap: time.Duration(gap) * time.Millisecond,
	}
	if token != "" {
		opts.Token = token
	}
	if proxies != "" {
		opts.ProxyURLs = parseProxies(proxies)
	}

	scraper := redditscraper.New(opts, progress)

	post, err := scraper.FetchSinglePost(subreddit, postID)
	if err != nil {
		log.Fatalf("FetchSinglePost failed: %v", err)
	}
	close(progress)

	nc := countAll(post.Comments)

	f, err := os.Create(outFile)
	if err != nil {
		log.Fatalf("Create output file: %v", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	if err := enc.Encode(post); err != nil {
		log.Fatalf("Write post: %v", err)
	}

	stat, _ := f.Stat()
	sizeMB := float64(stat.Size()) / 1024 / 1024

	log.Printf("━━━ Done ━━━")
	log.Printf("Post: %q by u/%s (%d points, %d comments fetched)", post.Title, post.Author, post.Score, nc)
	log.Printf("Output: %s (%.2f MB)", outFile, sizeMB)
}

func parsePostSource(value string) (redditscraper.PostSource, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return redditscraper.SourceAuto, nil
	case "reddit", "live":
		return redditscraper.SourceReddit, nil
	case "arctic", "arctic-shift", "archive":
		return redditscraper.SourceArctic, nil
	default:
		return "", fmt.Errorf("invalid -source %q (expected auto|reddit|arctic)", value)
	}
}

func parseSince(value string) (time.Time, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid -since %q (expected YYYY-MM-DD)", value)
}

func describeSource(s redditscraper.PostSource) string {
	switch s {
	case redditscraper.SourceArctic:
		return "arctic-shift"
	case redditscraper.SourceReddit:
		return "reddit-listings"
	default:
		return "auto (arctic→reddit fallback)"
	}
}

func describeSince(t time.Time) string {
	if t.IsZero() {
		return "(no cutoff)"
	}
	return t.Format("2006-01-02")
}

func toProxyURL(s string) string {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "socks5://") {
		return s
	}
	// host:port:user:pass format (Webshare, etc.)
	parts := strings.SplitN(s, ":", 4)
	if len(parts) == 4 {
		return fmt.Sprintf("http://%s:%s@%s:%s", parts[2], parts[3], parts[0], parts[1])
	}
	// host:port only
	if len(parts) == 2 {
		return "http://" + s
	}
	return "http://" + s
}
