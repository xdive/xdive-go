# xdive-go

The official Go client for the XDive web data API: scrape, crawl, map, search,
research and extract. Standard library only, Go 1.22 or newer.

```sh
go get github.com/xdive/xdive-go
```

## Configuration

| Setting  | Option           | Environment variable                      |
| -------- | ---------------- | ----------------------------------------- |
| API key  | `WithAPIKey`     | `XDIVE_API_KEY` (or `CRAWLER_API_KEY`)    |
| Base URL | `WithBaseURL`    | `XDIVE_BASE_URL` (or `CRAWLER_BASE_URL`)  |

Options win over the environment. The default base URL is
`xdive.DefaultBaseURL`. One API key identifies one organization; the client
looks the organization id up once with `GET /whoami` and caches it (skip the
lookup with `WithOrgID`). Other options: `WithHTTPClient`, `WithMaxRetries`
(default 3) and `WithTimeout` (per attempt, default 120s).

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"log"

	xdive "github.com/xdive/xdive-go"
)

func main() {
	ctx := context.Background()
	client, err := xdive.New() // reads XDIVE_API_KEY
	if err != nil {
		log.Fatal(err)
	}

	// Scrape one page.
	page, err := client.Scrape(ctx, "https://example.com", &xdive.ScrapeOptions{
		Formats: xdive.F(xdive.FormatMarkdown),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(xdive.PageMarkdown(page))

	// Crawl a site and wait for it to finish.
	record, err := client.Crawl(ctx, "https://example.com", &xdive.CrawlOptions{
		Formats: xdive.F(xdive.FormatMarkdown),
		Extra:   xdive.Map{"limit": 20},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(record.Status, len(record.Results))

	// Search the web.
	res, err := client.Search(ctx, "go context cancellation", xdive.Map{"limit": 5})
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range res.Results {
		fmt.Println(r.Title, r.URL)
	}

	// Research a question with verified citations.
	answer, err := client.Research(ctx, "What changed in Go 1.22?", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(answer.Answer, answer.Confidence)
	for _, c := range answer.Citations {
		fmt.Printf("[%d] %s\n", c.Number, c.URL)
	}
}
```

Every method takes a `context.Context` first and a pointer to an options struct
(`nil` for defaults). Options structs have an `Extra xdive.Map` for API fields
the SDK does not model, so new API fields never need an SDK upgrade.

### Methods

| Method | Returns |
| ------ | ------- |
| `Scrape(ctx, url, *ScrapeOptions)` | `*Page` |
| `StartCrawl(ctx, url, *CrawlOptions)` | crawl id |
| `Crawl(ctx, url, *CrawlOptions)` | `*CrawlRecord` (waits) |
| `GetCrawl(ctx, id)` | `*CrawlRecord` |
| `WaitForCrawl(ctx, id, *WaitOptions)` | `*CrawlRecord` (ETag and `Retry-After` aware) |
| `CancelCrawl(ctx, id)` | `error` |
| `CrawlErrors(ctx, id)` | `*CrawlErrors` (failed and robots.txt-blocked pages) |
| `BatchScrape(ctx, urls, *BatchOptions)` | `*CrawlRecord` (waits) |
| `StartBatchScrape(ctx, urls, *BatchOptions)` | job id, no waiting |
| `Map(ctx, url, *MapOptions)` | `[]string` |
| `Search(ctx, query, xdive.Map)` | `*SearchResponse` |
| `Research(ctx, query, *ResearchOptions)` | `*ResearchResult` |
| `Extract(ctx, urls, *ExtractOptions)` | `xdive.Map` |
| `LlmsTxt(ctx, url, *LlmsTxtOptions)` | `xdive.Map` |
| `Whoami(ctx)`, `OrgID(ctx)` | key details, organization id |

Every POST method also accepts trailing `CallOption`s.

## Error handling

Failed requests return `*xdive.Error`:

```go
_, err := client.Scrape(ctx, "https://example.com", nil)

var apiErr *xdive.Error
switch {
case err == nil:
case errors.Is(err, xdive.ErrQuotaExceeded): // HTTP 402
	log.Println("out of credits")
case errors.Is(err, xdive.ErrRateLimited): // retries exhausted
	var rl *xdive.RateLimitedError
	if errors.As(err, &rl) {
		log.Println("try again in", rl.RetryAfter)
	}
case errors.As(err, &apiErr):
	log.Println(apiErr.Status, apiErr.Code, apiErr.Message,
		apiErr.Retryable, apiErr.RequestID, apiErr.DocsURL)
}
```

`WaitForCrawl` (and `Crawl`, `BatchScrape`) return an error matching
`xdive.ErrCrawlTimeout` when the job does not finish in time; the job keeps
running, so call `CancelCrawl` if you no longer want it. Cancellation and
deadlines of your `ctx` are returned as `ctx.Err()`, also while the client is
waiting between retries.

Quote `RequestID` when contacting support.

## Retries and idempotency

- `429`, `503` with code `SERVER_BUSY` and `409` with code
  `IDEMPOTENCY_KEY_IN_PROGRESS` are retried after `Retry-After` seconds
  (default 1) up to `MaxRetries` times, then returned as `*RateLimitedError`.
- Transport errors and timeouts are retried for GET and POST with exponential
  backoff (1s, 2s, 4s, capped at 8s).
- No other status is retried.
- Every POST carries an `Idempotency-Key` (random UUID v4), generated once per
  call and reused by each retry, so a retry never runs or bills the work
  twice. Pass your own to make a whole operation safe to repeat:

```go
id, err := client.StartCrawl(ctx, "https://example.com", nil,
	xdive.WithIdempotencyKey("nightly-crawl-2026-01-01"))
```

## License

MIT
