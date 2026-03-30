package redditscraper

import "time"

// Subreddit holds metadata about a subreddit.
type Subreddit struct {
	Name              string    `json:"name"`
	Title             string    `json:"title"`
	Description       string    `json:"description"`
	PublicDescription string    `json:"public_description,omitempty"`
	Subscribers       int       `json:"subscribers"`
	CreatedUTC        time.Time `json:"created_utc"`
	Over18            bool      `json:"over_18"`
	SubredditType     string    `json:"subreddit_type"`
	ActiveUserCount   int       `json:"active_user_count,omitempty"`
}

// Post represents a single Reddit submission.
type Post struct {
	ID            string    `json:"id"`
	Subreddit     string    `json:"subreddit"`
	Title         string    `json:"title"`
	Author        string    `json:"author"`
	SelfText      string    `json:"selftext"`
	Score         int       `json:"score"`
	UpvoteRatio   float64   `json:"upvote_ratio"`
	NumComments   int       `json:"num_comments"`
	CreatedUTC    time.Time `json:"created_utc"`
	Permalink     string    `json:"permalink"`
	URL           string    `json:"url"`
	Domain        string    `json:"domain"`
	IsSelf        bool      `json:"is_self"`
	IsVideo       bool      `json:"is_video"`
	Over18        bool      `json:"over_18"`
	Stickied      bool      `json:"stickied"`
	Locked        bool      `json:"locked"`
	Archived      bool      `json:"archived"`
	Distinguished string    `json:"distinguished,omitempty"`
	LinkFlairText string    `json:"link_flair_text,omitempty"`
	Comments      []Comment `json:"comments,omitempty"`
}

// Comment represents a single comment in a post's comment tree.
type Comment struct {
	ID               string    `json:"id"`
	Author           string    `json:"author"`
	Body             string    `json:"body"`
	Score            int       `json:"score"`
	CreatedUTC       time.Time `json:"created_utc"`
	ParentID         string    `json:"parent_id"`
	Permalink        string    `json:"permalink"`
	Depth            int       `json:"depth"`
	IsSubmitter      bool      `json:"is_submitter"`
	Stickied         bool      `json:"stickied"`
	Distinguished    string    `json:"distinguished,omitempty"`
	Controversiality int       `json:"controversiality"`
	Edited           bool      `json:"edited"`
	Replies          []Comment `json:"replies,omitempty"`
}

// ScrapeResult is the complete output of [Scraper.ScrapeAll].
type ScrapeResult struct {
	Subreddit  Subreddit `json:"subreddit"`
	Posts      []Post    `json:"posts"`
	ScrapedAt  time.Time `json:"scraped_at"`
	TotalPosts int       `json:"total_posts"`
}

// Progress reports scraper state via the channel passed to [New].
type Progress struct {
	Phase        string `json:"phase"`         // "auth", "posts", or "comments"
	PostsFetched int    `json:"posts_fetched"`
	TotalPosts   int    `json:"total_posts"`   // set after all posts are collected
	PostID       string `json:"post_id"`       // set during comment fetching
	Message      string `json:"message"`
}
