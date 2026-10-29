// Package xdive is the official Go client for the XDive web data API:
// scrape, crawl, map, search, research and extract.
//
// Create a client with [New]. The API key comes from [WithAPIKey] or from the
// XDIVE_API_KEY environment variable (CRAWLER_API_KEY is accepted as a
// fallback); the base URL from [WithBaseURL], XDIVE_BASE_URL or
// CRAWLER_BASE_URL, in that order, defaulting to [DefaultBaseURL].
//
//	client, err := xdive.New()
//	if err != nil { ... }
//	page, err := client.Scrape(ctx, "https://example.com", nil)
//	fmt.Println(xdive.PageMarkdown(page))
//
// One API key identifies one organization. The client discovers the
// organization id once through GET /whoami and caches it; pass [WithOrgID] to
// skip the lookup.
//
// # Errors
//
// Failed requests return an *[Error] carrying the HTTP status, the API error
// code, the Retryable flag, the request id and the documentation URL. Use
// errors.As to read it and errors.Is with [ErrRateLimited], [ErrQuotaExceeded]
// or [ErrCrawlTimeout] to classify it.
//
// # Retries and idempotency
//
// The client waits for Retry-After and retries 429, 503 SERVER_BUSY and 409
// IDEMPOTENCY_KEY_IN_PROGRESS responses, and retries transport errors and
// timeouts of GET and POST requests with exponential backoff. Every POST
// carries an Idempotency-Key that all retries of that call share, so a retry
// never runs or bills the work twice. Override the key per call with
// [WithIdempotencyKey].
package xdive
