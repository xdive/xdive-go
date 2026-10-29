# Changelog

All notable changes to the `github.com/xdive/xdive-go` module. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the module
uses [Semantic Versioning](https://semver.org/).

## Unreleased

### Added

- First release of the official Go client, at feature parity with the
  JavaScript and Python SDKs: `Scrape`, `StartCrawl`, `GetCrawl`,
  `WaitForCrawl`, `Crawl`, `CancelCrawl`, `CrawlErrors`, `BatchScrape`,
  `StartBatchScrape`, `Map`, `Search`, `Research`, `Extract`, `LlmsTxt`,
  `Whoami` and `OrgID`, plus the `PageMarkdown` helper.
- Standard library only; requires Go 1.22 or newer.
- `*Error` (status, code, message, `Retryable`, request id, docs URL),
  `*RateLimitedError` (with `RetryAfter`), `*CrawlTimeoutError`, and the
  sentinels `ErrRateLimited`, `ErrQuotaExceeded`, `ErrCrawlTimeout` for
  `errors.Is`.
- Retries on 429, 503 `SERVER_BUSY` and 409 `IDEMPOTENCY_KEY_IN_PROGRESS`
  (honoring `Retry-After`) and on transport errors and timeouts for GET and
  POST with exponential backoff.
- Every POST carries an `Idempotency-Key` (random UUID v4) reused by all
  retries of that call; override it per call with `WithIdempotencyKey`.
- Configuration through `XDIVE_API_KEY` / `XDIVE_BASE_URL`, falling back to
  `CRAWLER_API_KEY` / `CRAWLER_BASE_URL`.
