package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	redditscraper "github.com/teslashibe/reddit-scraper"
)

func main() {
	subreddit := "Peptides"
	outFile := fmt.Sprintf("r_%s_%s.jsonl", subreddit, time.Now().Format("2006-01-02"))

	log.Printf("Scraping r/%s — posts + full comment trees", subreddit)
	log.Printf("Output: %s (JSONL, one post per line)", outFile)

	progress := make(chan redditscraper.Progress, 500)
	go func() {
		for p := range progress {
			log.Printf("[%s] %s", p.Phase, p.Message)
		}
	}()

	scraper := redditscraper.New(&redditscraper.Options{
		MinRequestGap: 500 * time.Millisecond,
	}, progress)

	log.Println("Phase 1: Fetching all posts...")
	posts, err := scraper.FetchPosts(subreddit)
	if err != nil {
		log.Fatalf("FetchPosts failed: %v", err)
	}
	log.Printf("Got %d unique posts. Phase 2: Fetching comments...", len(posts))

	f, err := os.Create(outFile)
	if err != nil {
		log.Fatalf("Create file: %v", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	totalComments := 0
	start := time.Now()

	for i := range posts {
		comments, selfText, err := scraper.FetchComments(subreddit, posts[i].ID)
		if err != nil {
			log.Printf("  warning: post %s comments failed: %v", posts[i].ID, err)
		}
		posts[i].Comments = comments
		if selfText != "" {
			posts[i].SelfText = selfText
		}

		nc := countAll(comments)
		totalComments += nc

		if err := enc.Encode(posts[i]); err != nil {
			log.Fatalf("Write post %s: %v", posts[i].ID, err)
		}

		if (i+1)%25 == 0 || i == len(posts)-1 {
			elapsed := time.Since(start)
			rate := float64(i+1) / elapsed.Seconds()
			eta := time.Duration(float64(len(posts)-i-1) / rate * float64(time.Second))
			log.Printf("  [%d/%d] %d comments so far | %.1f posts/sec | ETA %s",
				i+1, len(posts), totalComments, rate, eta.Round(time.Second))
		}
	}
	close(progress)

	stat, _ := f.Stat()
	sizeMB := float64(stat.Size()) / 1024 / 1024

	log.Printf("Done in %s", time.Since(start).Round(time.Second))
	log.Printf("Output: %s (%.1f MB)", outFile, sizeMB)
	log.Printf("Total: %d posts, %d comments", len(posts), totalComments)
}

func countAll(comments []redditscraper.Comment) int {
	n := len(comments)
	for _, c := range comments {
		n += countAll(c.Replies)
	}
	return n
}
