package xdive_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	xdive "github.com/xdive/xdive-go"
)

func ExampleNew() {
	// The key comes from XDIVE_API_KEY when WithAPIKey is not given.
	client, err := xdive.New(
		xdive.WithAPIKey("ck_live_..."),
		xdive.WithMaxRetries(5),
		xdive.WithTimeout(60*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}
	_ = client
}

func ExampleClient_Scrape() {
	client, _ := xdive.New()
	page, err := client.Scrape(context.Background(), "https://example.com", &xdive.ScrapeOptions{
		Formats: xdive.F(xdive.FormatMarkdown),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(xdive.PageMarkdown(page))
}

func ExampleClient_Crawl() {
	client, _ := xdive.New()
	record, err := client.Crawl(context.Background(), "https://example.com", &xdive.CrawlOptions{
		Formats: xdive.F(xdive.FormatMarkdown),
		Extra:   xdive.Map{"limit": 20},
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, page := range record.Results {
		fmt.Println(page.URL)
	}
}

func ExampleClient_StartCrawl() {
	client, _ := xdive.New()
	ctx := context.Background()
	id, err := client.StartCrawl(ctx, "https://example.com", nil)
	if err != nil {
		log.Fatal(err)
	}
	record, err := client.WaitForCrawl(ctx, id, &xdive.WaitOptions{Timeout: time.Minute})
	if errors.Is(err, xdive.ErrCrawlTimeout) {
		// Still running on the server; stop paying for it.
		_ = client.CancelCrawl(ctx, id)
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(record.Status, record.Pages)
}

func ExampleClient_Search() {
	client, _ := xdive.New()
	res, err := client.Search(context.Background(), "go context cancellation", xdive.Map{"limit": 5})
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range res.Results {
		fmt.Println(r.Title, r.URL)
	}
}

func ExampleClient_Research() {
	client, _ := xdive.New()
	res, err := client.Research(context.Background(), "What changed in Go 1.22?", &xdive.ResearchOptions{Depth: "quick"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Answer)
	for _, c := range res.Citations {
		fmt.Printf("[%d] %s\n", c.Number, c.URL)
	}
}

func ExampleClient_Extract() {
	client, _ := xdive.New()
	out, err := client.Extract(context.Background(), []string{"https://example.com/pricing"}, &xdive.ExtractOptions{
		Prompt: "List the plan names and prices",
		Schema: xdive.Map{"type": "object"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out)
}

func ExampleError() {
	client, _ := xdive.New()
	_, err := client.Scrape(context.Background(), "https://example.com", nil)

	var apiErr *xdive.Error
	switch {
	case err == nil:
	case errors.Is(err, xdive.ErrQuotaExceeded):
		fmt.Println("out of credits")
	case errors.Is(err, xdive.ErrRateLimited):
		var rl *xdive.RateLimitedError
		if errors.As(err, &rl) {
			fmt.Println("retry in", rl.RetryAfter)
		}
	case errors.As(err, &apiErr):
		fmt.Println(apiErr.Status, apiErr.Code, apiErr.RequestID, apiErr.Retryable, apiErr.DocsURL)
	default:
		fmt.Println("transport or context error:", err)
	}
}

func ExampleWithIdempotencyKey() {
	client, _ := xdive.New()
	// Re-running this program with the same key replays the first response
	// instead of starting a second crawl.
	id, err := client.StartCrawl(context.Background(), "https://example.com", nil,
		xdive.WithIdempotencyKey("nightly-crawl-2026-01-01"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(id)
}
