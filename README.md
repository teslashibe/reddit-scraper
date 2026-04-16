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

Reddit limits every listing to ~1,000 posts. This package bypasses that two ways:

1. **Live Reddit, multi-sort merge** — queries the same subreddit through **12 different sort/time strategies**, dedupes by post ID. Yields **~2,000–5,000 unique posts** per sub. Always live, always authoritative.
2. **Arctic Shift archive** (default) — queries the community-maintained Pushshift replacement for the full historical dump. Yields **tens of thousands of posts** going back to subreddit creation. Empirically: 67,144 posts for r/Peptides covering 12 years, ~10s per 1,000 posts.

Both modes return the same `Post` struct with full comment trees fetched from live Reddit, so you can swap backends without changing downstream code.

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

### Pick a backend (default: auto)

```go
// Force Arctic Shift archive — unlocks tens of thousands of posts
scraper := redditscraper.New(&redditscraper.Options{
    Source:    redditscraper.SourceArctic,
    PostLimit: 10000,
}, nil)

// Force Reddit live listings only — no third-party dependency
scraper := redditscraper.New(&redditscraper.Options{
    Source: redditscraper.SourceReddit,
}, nil)

// Default: try Arctic Shift first, fall back to Reddit on error
scraper := redditscraper.New(nil, nil)
```

### Filter by date range

`OldestPost` is honoured by both backends (server-side filter on Arctic Shift, client-side on Reddit listings):

```go
scraper := redditscraper.New(&redditscraper.Options{
    OldestPost: time.Now().AddDate(-1, 0, 0), // last year
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
| `Source` | `PostSource` | `SourceAuto` | `SourceAuto` (try Arctic Shift, fall back to Reddit), `SourceArctic`, or `SourceReddit` |
| `OldestPost` | `time.Time` | `zero` (no cutoff) | Earliest post date to fetch — server-side filter on Arctic Shift |
| `Token` | `string` | `""` | Reddit session cookie for authenticated requests |
| `UserAgent` | `string` | Chrome UA | Custom User-Agent string |
| `RequestTimeout` | `time.Duration` | `30s` | HTTP timeout per request |
| `MinRequestGap` | `time.Duration` | `650ms` | Rate-limit pause between Reddit requests |
| `ArcticAPIBase` | `string` | `https://arctic-shift.photon-reddit.com` | Override Arctic Shift base URL |
| `ArcticRequestGap` | `time.Duration` | `200ms` | Pause between Arctic Shift requests |
| `PostLimit` | `int` | `0` (all) | Max posts to return |
| `CommentDepth` | `int` | `500` | Max comment nesting depth |
| `SkipComments` | `bool` | `false` | Skip comment fetching entirely |
| `ProxyURLs` | `[]string` | `nil` | HTTP/SOCKS5 proxies to rotate through (Reddit-side only) |

---

## How it works

### Arctic Shift backend (default, deepest reach)

[Arctic Shift](https://arctic-shift.photon-reddit.com) is a community-maintained continuous archive of Reddit, replacing the dead Pushshift service. The scraper paginates it newest-first via cursor-based `before=<unix_ts>` queries at 100 posts/request. There's effectively no per-sub cap — for r/Peptides we pulled 67,144 posts going back to 2014. Comments are still fetched live from Reddit using the post IDs Arctic Shift returns.

### Reddit live backend (fallback)

Reddit caps each listing at ~1,000 results. Different sort/time combinations surface different posts:

| Strategy | What it finds |
|:---|:---|
| `new` | Most recent ~1,000 posts |
| `top/all` `top/year` `top/month` `top/week` `top/day` | Highest-voted in each window |
| `controversial/all` `controversial/year` `controversial/month` `controversial/week` | Most controversial in each window |
| `hot`, `rising` | Currently trending |

12 strategies merge into ~2,000–5,000 unique posts depending on subreddit volume.

**Built-in protections (both backends):**
- Respects `X-Ratelimit-*` headers and retries on 429s
- Exponential backoff on consecutive errors
- Stale-page detection skips strategies that stop finding new posts
- Expands Reddit's collapsed "more comments" threads automatically
- Auto-fallback from Arctic Shift to Reddit if the archive is unreachable

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

## CLI

```bash
go install github.com/teslashibe/reddit-scraper/cmd/scrape@latest

# Default: auto backend, posts + comments
scrape -sub Peptides -out ./data

# Tens of thousands of posts, metadata only (much faster)
scrape -sub Peptides -source arctic -max 50000 -skip-comments -out ./data

# Last year only, with proxies
scrape -sub Fitness -since 2025-04-16 -proxies proxies.txt -out ./data

# Force the live Reddit backend (no Arctic Shift dependency)
scrape -sub Peptides -source reddit -out ./data
```

| Flag | Default | Description |
|:---|:---|:---|
| `-source` | `auto` | `auto`, `arctic`, or `reddit` |
| `-since` | `""` | Date cutoff (YYYY-MM-DD) |
| `-max` | `0` (all) | Cap on total posts |
| `-skip-comments` | `false` | Posts-only mode |
| `-gap` | `1000` | ms between Reddit requests |
| `-proxies` | `""` | Proxy file or comma list |
| `-token` | `""` | Reddit `token_v2` cookie |

## Testing

```bash
# Quick smoke tests (hits live Reddit + Arctic Shift APIs)
go test -v -short ./...

# Full deep-pagination test (~60s)
go test -v -run TestMultiSort -timeout 120s

# With authentication
REDDIT_TOKEN=your_token go test -v -run TestWithToken
```

---

## License

[MIT](LICENSE)
