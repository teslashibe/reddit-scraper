package redditscraper_test

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	redditscraper "github.com/teslashibe/reddit-scraper"
)

func Example() {
	s := redditscraper.New(&redditscraper.Options{
		PostLimit:    3,
		SkipComments: true,
	}, nil)

	result, err := s.ScrapeAll("golang")
	if err != nil {
		log.Fatal(err)
	}

	for _, p := range result.Posts {
		fmt.Printf("%s — %s (score %d)\n", p.ID, p.Title, p.Score)
	}
}

func Example_withProgress() {
	progress := make(chan redditscraper.Progress, 100)
	go func() {
		for p := range progress {
			log.Printf("[%s] %s", p.Phase, p.Message)
		}
	}()

	s := redditscraper.New(&redditscraper.Options{
		PostLimit: 50,
	}, progress)

	result, err := s.ScrapeAll("golang")
	close(progress)
	if err != nil {
		log.Fatal(err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result)
}
