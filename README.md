<p align="center">
  <h1 align="center">reddit-scraper</h1>
  <p align="center">
    <strong>Scrape any subreddit's posts + full comment trees. No API keys. Zero dependencies.</strong>
  </p>
  <p align="center">
    <a href="https://pkg.go.dev/github.com/teslashibe/reddit-scraper"><img src="https://pkg.go.dev/badge/github.com/teslashibe/reddit-scraper.svg" alt="Go Reference"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="MIT License"></a>
    <a href="https://goreportcard.com/report/github.com/teslashibe/reddit-scraper"><img src="https://goreportcard.com/badge/github.com/teslashibe/reddit-scraper" alt="Go Report Card"></a>
  </p>
</p>

---

## Why this exists

Reddit limits every listing to ~1,000 posts. This package bypasses that by querying the same subreddit through **7 different sort/time strategies**, merging and deduplicating by post ID. Result: **2,000+ unique posts** spanning years of history from a single call — with full comment trees included.

---

## Install

```bash
go get github.com/teslashibe/reddit-scraper
```

Requires **Go 1.21+**. Zero external dependencies — stdlib only.

---

## Get started in 30 seconds

```go
package main

import (
    "encoding/json"
    "log"
    "os"

    redditscraper "github.com/teslashibe/reddit-scraper"
)

func main() {
    scraper := redditscraper.New(nil, nil)

    result, err := scraper.ScrapeAll("golang")
    if err != nil {
        log.Fatal(err)
    }

    log.Printf("Got %d posts from r/%s", result.TotalPosts, result.Subreddit.Name)

    json.NewEncoder(os.Stdout).Encode(result)
}
```

That's it. Every post. Every comment. Structured JSON.

---

## Usage

### Scrape everything

```go
result, err := scraper.ScrapeAll("Peptides")
```

Returns a `*ScrapeResult` containing:
- `result.Subreddit` — sub metadata (name, subscribers, description)
- `result.Posts` — all posts, each with `.Comments` fully populated
- `result.TotalPosts` — count
- `result.ScrapedAt` — timestamp

### Posts only (skip comments)

Much faster for large subreddits when you only need post metadata:

```go
scraper := redditscraper.New(&redditscraper.Options{
    SkipComments: true,
}, nil)
result, err := scraper.ScrapeAll("Peptides")
```

### Limit post count

```go
scraper := redditscraper.New(&redditscraper.Options{
    PostLimit: 100,
}, nil)
```

### Track progress in real time

```go
progress := make(chan redditscraper.Progress, 100)

go func() {
    for p := range progress {
        log.Printf("[%s] %s", p.Phase, p.Message)
    }
}()

scraper := redditscraper.New(nil, progress)
result, err := scraper.ScrapeAll("golang")
close(progress)
```

Outputs:
```
[posts]    fetched page (sort=new, 100 new, 100 total unique)
[posts]    fetched page (sort=new, 100 new, 200 total unique)
[posts]    after new: 958 unique posts total (958 new from this sort)
[posts]    after top/all: 1403 unique posts total (445 new from this sort)
[comments] fetching comments for post 1/1403 (abc123)
```

### Use individual methods

```go
scraper := redditscraper.New(nil, nil)

sub, err := scraper.FetchSubreddit("golang")
posts, err := scraper.FetchPosts("golang")
comments, selfText, err := scraper.FetchComments("golang", "abc123")
```

### Authenticated scraping (NSFW / age-gated)

```go
scraper := redditscraper.New(&redditscraper.Options{
    Token: "your-token_v2-value",
}, nil)
```

<details>
<summary>How to get your token</summary>

1. Log in to [reddit.com](https://reddit.com) in your browser
2. Open **DevTools** → **Application** → **Cookies** → `https://www.reddit.com`
3. Find and copy the value of `token_v2`
4. Pass it as `Options.Token`

</details>

---

## Options reference

| Option | Type | Default | Description |
|:---|:---|:---|:---|
| `Token` | `string` | `""` | Reddit session cookie for authenticated requests |
| `UserAgent` | `string` | Chrome UA | Custom User-Agent string |
| `RequestTimeout` | `time.Duration` | `30s` | HTTP timeout per request |
| `MinRequestGap` | `time.Duration` | `650ms` | Rate-limit pause between requests |
| `PostLimit` | `int` | `0` (all) | Max posts to return |
| `CommentDepth` | `int` | `500` | Max comment nesting depth |
| `SkipComments` | `bool` | `false` | Skip comment fetching entirely |

---

## How it works

Reddit caps each listing at ~1,000 results. Different sort orders surface different posts:

| Strategy | What it finds |
|:---|:---|
| `new` | Most recent ~1,000 posts |
| `top/all` | Highest-voted posts of all time |
| `top/year` | Top posts this year |
| `top/month` | Top posts this month |
| `controversial/all` | Most controversial ever |
| `controversial/year` | Most controversial this year |
| `hot` | Currently trending |

The scraper exhausts each strategy, merges by post ID, and returns the combined set sorted newest-first.

**Built-in protections:**
- Respects `X-Ratelimit-*` headers and retries on 429s
- Exponential backoff on consecutive errors
- Stale-page detection skips strategies that stop finding new posts
- Expands Reddit's collapsed "more comments" threads automatically

---

## Data model

```
ScrapeResult
├── Subreddit          (name, subscribers, description, ...)
├── Posts[]
│   ├── ID, Title, Author, SelfText, Score, ...
│   └── Comments[]
│       ├── ID, Author, Body, Score, Depth, ...
│       └── Replies[]     ← recursive
│           └── ...
├── TotalPosts
└── ScrapedAt
```

Full JSON output:

```json
{
  "subreddit": {
    "name": "golang",
    "subscribers": 285000
  },
  "posts": [
    {
      "id": "abc123",
      "title": "Understanding Go interfaces",
      "author": "gopher42",
      "selftext": "Let me explain...",
      "score": 187,
      "num_comments": 34,
      "comments": [
        {
          "id": "xyz789",
          "author": "commenter",
          "body": "Great explanation!",
          "score": 42,
          "depth": 0,
          "replies": [
            {
              "id": "def456",
              "body": "Agreed, very clear.",
              "depth": 1
            }
          ]
        }
      ]
    }
  ],
  "total_posts": 2100,
  "scraped_at": "2026-03-30T12:00:00Z"
}
```

---

## Testing

```bash
# Quick smoke tests (hits live Reddit API)
go test -v -short ./...

# Full deep-pagination test (~60s, fetches 2000+ posts)
go test -v -run TestMultiSort -timeout 120s

# With authentication
REDDIT_TOKEN=your_token go test -v -run TestWithToken
```

---

## License

[MIT](LICENSE)
