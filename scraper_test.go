package redditscraper

import (
	"os"
	"testing"
	"time"
)

const testSubreddit = "Peptides"

func TestFetchSubreddit(t *testing.T) {
	s := New(nil, nil)
	sub, err := s.FetchSubreddit(testSubreddit)
	if err != nil {
		t.Fatalf("FetchSubreddit: %v", err)
	}

	if sub.Name != testSubreddit {
		t.Errorf("expected name %s, got %q", testSubreddit, sub.Name)
	}
	if sub.Subscribers == 0 {
		t.Error("expected non-zero subscribers")
	}
	if sub.CreatedUTC.IsZero() {
		t.Error("expected non-zero created_utc")
	}

	t.Logf("Subreddit: %s (%d subscribers, created %s)", sub.Name, sub.Subscribers, sub.CreatedUTC.Format(time.RFC3339))
}

func TestFetchPosts(t *testing.T) {
	s := New(&Options{PostLimit: 5}, nil)
	posts, err := s.FetchPosts(testSubreddit)
	if err != nil {
		t.Fatalf("FetchPosts: %v", err)
	}
	if len(posts) == 0 {
		t.Fatal("expected at least one post")
	}
	if len(posts) > 5 {
		t.Errorf("expected at most 5 posts, got %d", len(posts))
	}

	for i, p := range posts {
		if p.ID == "" {
			t.Errorf("post %d: missing ID", i)
		}
		if p.Title == "" {
			t.Errorf("post %d: missing title", i)
		}
		t.Logf("Post %d: [%s] %q by %s (score: %d, comments: %d)",
			i, p.ID, p.Title, p.Author, p.Score, p.NumComments)
	}
}

func TestFetchComments(t *testing.T) {
	s := New(&Options{PostLimit: 25}, nil)
	posts, err := s.FetchPosts(testSubreddit)
	if err != nil {
		t.Fatalf("FetchPosts: %v", err)
	}

	var target Post
	for _, p := range posts {
		if p.NumComments > 0 {
			target = p
			break
		}
	}
	if target.ID == "" {
		t.Skip("no posts with comments found")
	}

	t.Logf("Fetching comments for %q (%s, %d expected)", target.Title, target.ID, target.NumComments)

	comments, selfText, err := s.FetchComments(testSubreddit, target.ID)
	if err != nil {
		t.Fatalf("FetchComments: %v", err)
	}
	if selfText != "" {
		t.Logf("Selftext: %.200s", selfText)
	}
	if len(comments) == 0 {
		t.Fatal("expected at least one comment")
	}

	t.Logf("Got %d top-level comments (%d total)", len(comments), countComments(comments))

	c := comments[0]
	if c.ID == "" || c.Body == "" {
		t.Error("first comment missing ID or body")
	}
	t.Logf("First comment by %s (score %d): %.120s", c.Author, c.Score, c.Body)
}

func TestScrapeAll(t *testing.T) {
	progress := make(chan Progress, 200)
	s := New(&Options{PostLimit: 3, SkipComments: false}, progress)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progress {
			t.Logf("[%s] %s", p.Phase, p.Message)
		}
	}()

	result, err := s.ScrapeAll(testSubreddit)
	close(progress)
	<-done

	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}
	if result.Subreddit.Name != testSubreddit {
		t.Errorf("expected subreddit %s, got %q", testSubreddit, result.Subreddit.Name)
	}
	if len(result.Posts) == 0 {
		t.Fatal("expected at least one post")
	}
	if result.TotalPosts != len(result.Posts) {
		t.Errorf("TotalPosts mismatch: %d vs %d", result.TotalPosts, len(result.Posts))
	}

	withComments := 0
	total := 0
	for _, p := range result.Posts {
		if len(p.Comments) > 0 {
			withComments++
			total += countComments(p.Comments)
		}
	}
	t.Logf("Scraped %d posts, %d with comments, %d total comments",
		len(result.Posts), withComments, total)
}

func TestMultiSortPagination(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping deep pagination test in short mode")
	}

	progress := make(chan Progress, 2000)
	s := New(&Options{PostLimit: 0}, progress)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progress {
			t.Logf("[%s] %s", p.Phase, p.Message)
		}
	}()

	posts, err := s.FetchPosts(testSubreddit)
	close(progress)
	<-done

	if err != nil {
		t.Fatalf("FetchPosts: %v", err)
	}

	t.Logf("Total unique posts: %d", len(posts))
	if len(posts) < 1000 {
		t.Errorf("expected at least 1000 posts with multi-sort, got %d", len(posts))
	}

	if len(posts) > 0 {
		t.Logf("Newest: [%s] %q (%s)", posts[0].ID, posts[0].Title, posts[0].CreatedUTC.Format(time.RFC3339))
		t.Logf("Oldest: [%s] %q (%s)", posts[len(posts)-1].ID, posts[len(posts)-1].Title, posts[len(posts)-1].CreatedUTC.Format(time.RFC3339))

		years := make(map[int]int)
		for _, p := range posts {
			years[p.CreatedUTC.Year()]++
		}
		for y := 2018; y <= 2030; y++ {
			if c, ok := years[y]; ok {
				t.Logf("  %d: %d posts", y, c)
			}
		}
	}
}

func TestWithToken(t *testing.T) {
	token := os.Getenv("REDDIT_TOKEN")
	if token == "" {
		t.Skip("set REDDIT_TOKEN to run")
	}

	progress := make(chan Progress, 100)
	s := New(&Options{Token: token, PostLimit: 5}, progress)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progress {
			t.Logf("[%s] %s", p.Phase, p.Message)
		}
	}()

	result, err := s.ScrapeAll(testSubreddit)
	close(progress)
	<-done

	if err != nil {
		t.Fatalf("ScrapeAll with token: %v", err)
	}
	if !s.IsAuthenticated() {
		t.Error("expected authenticated")
	}
	t.Logf("Authenticated scrape: %d posts", len(result.Posts))
}

func countComments(comments []Comment) int {
	n := len(comments)
	for _, c := range comments {
		n += countComments(c.Replies)
	}
	return n
}
