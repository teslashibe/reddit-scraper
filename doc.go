// Package redditscraper scrapes posts and full comment trees from any
// public subreddit — no API keys, no OAuth, zero dependencies.
//
// It uses a multi-sort strategy across Reddit's public JSON endpoints
// (new, top, controversial, hot) to collect 2-3x more posts than any
// single listing allows, then deduplicates by post ID.
//
// # Quick start
//
//	s := redditscraper.New(nil, nil)
//	result, err := s.ScrapeAll("golang")
//	// result.Posts — every post, each with .Comments fully populated
//
// # With options
//
//	s := redditscraper.New(&redditscraper.Options{
//		PostLimit:    500,   // cap at 500 posts
//		SkipComments: true,  // posts only, no comment trees
//	}, nil)
//
// # Progress tracking
//
//	progress := make(chan redditscraper.Progress, 100)
//	go func() {
//		for p := range progress {
//			log.Printf("[%s] %s", p.Phase, p.Message)
//		}
//	}()
//	s := redditscraper.New(nil, progress)
//	result, _ := s.ScrapeAll("golang")
//	close(progress)
package redditscraper
