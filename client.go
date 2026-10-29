package xdive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the API origin used when none is configured.
const DefaultBaseURL = "https://xdive.dev/api"

const (
	defaultTimeout    = 120 * time.Second
	defaultMaxRetries = 3
)

// Client talks to the XDive API. It is safe for concurrent use.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	timeout    time.Duration
	maxRetries int

	mu    sync.Mutex
	orgID string

	// backoffBase is the first transport-error backoff (1s); tests shrink it.
	backoffBase time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the organization API key (default: XDIVE_API_KEY, then CRAWLER_API_KEY).
func WithAPIKey(key string) Option { return func(c *Client) { c.apiKey = key } }

// WithBaseURL sets the API origin including /api (default: XDIVE_BASE_URL,
// CRAWLER_BASE_URL, then DefaultBaseURL).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithOrgID skips the GET /whoami discovery of the organization id.
func WithOrgID(id string) Option { return func(c *Client) { c.orgID = id } }

// WithHTTPClient sets the http.Client used for requests.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithMaxRetries sets how many times a request is retried (default 3, 0 disables).
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n < 0 {
			n = 0
		}
		c.maxRetries = n
	}
}

// WithTimeout sets the per-attempt request timeout (default 120s).
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// New creates a Client. It returns ErrNoAPIKey when no key is configured.
func New(opts ...Option) (*Client, error) {
	c := &Client{
		timeout:     defaultTimeout,
		maxRetries:  defaultMaxRetries,
		backoffBase: time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	if c.apiKey == "" {
		c.apiKey = firstEnv("XDIVE_API_KEY", "CRAWLER_API_KEY")
	}
	if c.apiKey == "" {
		return nil, ErrNoAPIKey
	}
	if c.baseURL == "" {
		c.baseURL = firstEnv("XDIVE_BASE_URL", "CRAWLER_BASE_URL")
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	c.baseURL = strings.TrimRight(c.baseURL, "/")
	if c.httpClient == nil {
		c.httpClient = &http.Client{}
	}
	return c, nil
}

// BaseURL returns the configured API origin.
func (c *Client) BaseURL() string { return c.baseURL }

// CallOption customizes one API call.
type CallOption func(*callConfig)

type callConfig struct{ idempotencyKey string }

// WithIdempotencyKey sets the Idempotency-Key of a POST call instead of a
// generated one. Calls with the same key return the first call's response.
func WithIdempotencyKey(key string) CallOption {
	return func(c *callConfig) { c.idempotencyKey = key }
}

// newIdempotencyKey returns a random UUID v4.
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms.
		panic("xdive: crypto/rand: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// --- transport -----------------------------------------------------------------

type response struct {
	status int
	header http.Header
	body   []byte
}

type errorBody struct {
	Message   string
	Code      string
	Retryable bool
	DocsURL   string
}

func parseErrorBody(status int, raw []byte) errorBody {
	eb := errorBody{Message: http.StatusText(status)}
	var b struct {
		Error     any    `json:"error"`
		Message   string `json:"message"`
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
		DocsURL   string `json:"docs_url"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return eb
	}
	switch e := b.Error.(type) {
	case string:
		if e != "" {
			eb.Message = e
		}
	}
	if b.Message != "" && (b.Error == nil || b.Error == "") {
		eb.Message = b.Message
	}
	eb.Code, eb.Retryable, eb.DocsURL = b.Code, b.Retryable, b.DocsURL
	return eb
}

func isOverload(status int, eb errorBody) bool {
	return status == 429 ||
		(status == 503 && eb.Code == "SERVER_BUSY") ||
		(status == 409 && eb.Code == "IDEMPOTENCY_KEY_IN_PROGRESS")
}

func retryAfter(h http.Header, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return fallback
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || secs < 0 || secs > 1e9 {
		return fallback
	}
	return time.Duration(secs * float64(time.Second))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) backoff(attempt int) time.Duration {
	d := c.backoffBase << uint(attempt)
	if max := 8 * c.backoffBase; d > max || d <= 0 {
		d = max
	}
	return d
}

// do sends one logical request with retries. body is JSON-encoded when non-nil.
// Statuses below 400 (including 304) are returned; others become errors.
func (c *Client) do(ctx context.Context, method, path string, body any, extra http.Header, cfg callConfig) (*response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("xdive: encoding request: %w", err)
		}
	}
	idempotent := method == http.MethodGet || method == http.MethodPost
	key := ""
	if method == http.MethodPost {
		key = cfg.idempotencyKey
		if key == "" {
			key = newIdempotencyKey()
		}
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := c.attempt(ctx, method, path, payload, key, extra)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if idempotent && attempt < c.maxRetries {
				if serr := sleep(ctx, c.backoff(attempt)); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, err
		}
		if resp.status < 400 {
			return resp, nil
		}
		eb := parseErrorBody(resp.status, resp.body)
		apiErr := &Error{
			Status:    resp.status,
			Code:      eb.Code,
			Message:   eb.Message,
			Retryable: eb.Retryable,
			RequestID: resp.header.Get("X-Request-ID"),
			DocsURL:   eb.DocsURL,
		}
		if isOverload(resp.status, eb) {
			wait := retryAfter(resp.header, time.Second)
			if attempt < c.maxRetries {
				if serr := sleep(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
			apiErr.Retryable = true
			return nil, &RateLimitedError{Err: apiErr, RetryAfter: wait}
		}
		return nil, apiErr
	}
}

func (c *Client) attempt(ctx context.Context, method, path string, payload []byte, key string, extra http.Header) (*response, error) {
	actx, cancel := ctx, context.CancelFunc(func() {})
	if c.timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, c.timeout)
	}
	defer cancel()
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(actx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "xdive-go/"+Version)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return &response{status: res.StatusCode, header: res.Header, body: data}, nil
}

func (c *Client) callCfg(opts []CallOption) callConfig {
	var cfg callConfig
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

func (c *Client) post(ctx context.Context, path string, payload Map, out any, opts []CallOption) error {
	resp, err := c.do(ctx, http.MethodPost, path, payload, nil, c.callCfg(opts))
	if err != nil {
		return err
	}
	return decode(resp.body, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil, nil, callConfig{})
	if err != nil {
		return err
	}
	return decode(resp.body, out)
}

func decode(body []byte, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("xdive: decoding response: %w", err)
	}
	return nil
}

// OrgID returns the organization the API key belongs to, discovering it with
// GET /whoami on first use and caching it.
func (c *Client) OrgID(ctx context.Context) (string, error) {
	c.mu.Lock()
	id := c.orgID
	c.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var who struct {
		OrgID string `json:"orgId"`
	}
	if err := c.get(ctx, "/whoami", &who); err != nil {
		return "", err
	}
	if who.OrgID == "" {
		return "", errors.New("xdive: /whoami did not return an orgId")
	}
	c.mu.Lock()
	c.orgID = who.OrgID
	c.mu.Unlock()
	return who.OrgID, nil
}

func (c *Client) org(ctx context.Context, suffix string) (string, error) {
	id, err := c.OrgID(ctx)
	if err != nil {
		return "", err
	}
	return "/orgs/" + url.PathEscape(id) + suffix, nil
}

// Whoami returns who the API key is: organization, scopes, plan.
func (c *Client) Whoami(ctx context.Context) (Map, error) {
	var out Map
	err := c.get(ctx, "/whoami", &out)
	return out, err
}

// payload merges base (dropping zero-valued entries marked nil) and extra;
// extra wins, like the JS client's rest spread.
func payload(base Map, extra Map) Map {
	out := Map{}
	for k, v := range base {
		if v != nil {
			out[k] = v
		}
	}
	for k, v := range extra {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

func formatsOrNil(f []Format) any {
	if len(f) == 0 {
		return nil
	}
	return f
}

// --- pages ---------------------------------------------------------------------

// ScrapeOptions are the fields of a scrape request.
type ScrapeOptions struct {
	Formats []Format
	// Extra holds any other API fields (e.g. "only_main_content", "wait_for").
	Extra Map
}

// Scrape scrapes one page synchronously. opts may be nil.
func (c *Client) Scrape(ctx context.Context, pageURL string, opts *ScrapeOptions, call ...CallOption) (*Page, error) {
	if opts == nil {
		opts = &ScrapeOptions{}
	}
	path, err := c.org(ctx, "/scrape")
	if err != nil {
		return nil, err
	}
	var out struct {
		Data Page `json:"data"`
	}
	if err := c.post(ctx, path, payload(Map{"url": pageURL, "formats": formatsOrNil(opts.Formats)}, opts.Extra), &out, call); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// CrawlOptions are the fields of a crawl request.
type CrawlOptions struct {
	Formats []Format
	// Extra holds any other API fields (e.g. "limit", "max_depth").
	Extra Map
	// Timeout and PollInterval apply to Crawl only (defaults 10m and 2s).
	Timeout      time.Duration
	PollInterval time.Duration
}

// StartCrawl starts a crawl and returns its id. opts may be nil.
func (c *Client) StartCrawl(ctx context.Context, siteURL string, opts *CrawlOptions, call ...CallOption) (string, error) {
	if opts == nil {
		opts = &CrawlOptions{}
	}
	path, err := c.org(ctx, "/crawl")
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, path, payload(Map{"url": siteURL, "formats": formatsOrNil(opts.Formats)}, opts.Extra), &out, call); err != nil {
		return "", err
	}
	return out.ID, nil
}

// GetCrawl returns the status and, once finished, the results of a crawl.
func (c *Client) GetCrawl(ctx context.Context, crawlID string) (*CrawlRecord, error) {
	path, err := c.org(ctx, "/crawls/"+url.PathEscape(crawlID))
	if err != nil {
		return nil, err
	}
	var out CrawlRecord
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelCrawl cancels a queued or running crawl (or batch scrape).
func (c *Client) CancelCrawl(ctx context.Context, crawlID string) error {
	path, err := c.org(ctx, "/crawls/"+url.PathEscape(crawlID))
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodDelete, path, nil, nil, callConfig{})
	return err
}

// CrawlErrors returns the pages of a crawl that failed and those robots.txt blocked.
func (c *Client) CrawlErrors(ctx context.Context, crawlID string) (*CrawlErrors, error) {
	path, err := c.org(ctx, "/crawls/"+url.PathEscape(crawlID)+"/errors")
	if err != nil {
		return nil, err
	}
	var out CrawlErrors
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitOptions control WaitForCrawl.
type WaitOptions struct {
	// Timeout bounds the whole wait (default 10 minutes).
	Timeout time.Duration
	// PollInterval is used when the server sends no Retry-After (default 2s).
	PollInterval time.Duration
}

// WaitForCrawl polls a crawl until it finishes. It sends If-None-Match so an
// unchanged crawl costs a bodiless 304, and follows the server's Retry-After.
// It returns a *CrawlTimeoutError (errors.Is ErrCrawlTimeout) when the crawl
// does not finish in time, or ctx.Err() when ctx is cancelled. opts may be nil.
func (c *Client) WaitForCrawl(ctx context.Context, crawlID string, opts *WaitOptions) (*CrawlRecord, error) {
	timeout, interval := 10*time.Minute, 2*time.Second
	if opts != nil {
		if opts.Timeout > 0 {
			timeout = opts.Timeout
		}
		if opts.PollInterval > 0 {
			interval = opts.PollInterval
		}
	}
	path, err := c.org(ctx, "/crawls/"+url.PathEscape(crawlID))
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	etag := ""
	for {
		var extra http.Header
		if etag != "" {
			extra = http.Header{"If-None-Match": {etag}}
		}
		resp, err := c.do(ctx, http.MethodGet, path, nil, extra, callConfig{})
		if err != nil {
			return nil, err
		}
		if resp.status != http.StatusNotModified {
			var rec CrawlRecord
			if err := decode(resp.body, &rec); err != nil {
				return nil, err
			}
			etag = resp.header.Get("ETag")
			if isTerminal(rec.Status) {
				return &rec, nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, newCrawlTimeout(crawlID, timeout)
		}
		wait := retryAfter(resp.header, interval)
		if wait > remaining {
			wait = remaining
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// Crawl crawls a site and waits for the result. opts may be nil.
func (c *Client) Crawl(ctx context.Context, siteURL string, opts *CrawlOptions, call ...CallOption) (*CrawlRecord, error) {
	id, err := c.StartCrawl(ctx, siteURL, opts, call...)
	if err != nil {
		return nil, err
	}
	var w *WaitOptions
	if opts != nil {
		w = &WaitOptions{Timeout: opts.Timeout, PollInterval: opts.PollInterval}
	}
	return c.WaitForCrawl(ctx, id, w)
}

// BatchOptions are the fields of a batch scrape request.
type BatchOptions struct {
	Formats []Format
	Extra   Map
	// Timeout bounds the wait in BatchScrape (default 10 minutes).
	Timeout time.Duration
}

// StartBatchScrape starts a batch scrape of up to 100 URLs and returns the job
// id at once (follow it with WaitForCrawl, GetCrawl, CancelCrawl). opts may be nil.
func (c *Client) StartBatchScrape(ctx context.Context, urls []string, opts *BatchOptions, call ...CallOption) (string, error) {
	if opts == nil {
		opts = &BatchOptions{}
	}
	path, err := c.org(ctx, "/batch/scrape")
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, path, payload(Map{"urls": urls, "formats": formatsOrNil(opts.Formats)}, opts.Extra), &out, call); err != nil {
		return "", err
	}
	return out.ID, nil
}

// BatchScrape scrapes up to 100 URLs through the job pipeline and waits for the job.
func (c *Client) BatchScrape(ctx context.Context, urls []string, opts *BatchOptions, call ...CallOption) (*CrawlRecord, error) {
	id, err := c.StartBatchScrape(ctx, urls, opts, call...)
	if err != nil {
		return nil, err
	}
	var w *WaitOptions
	if opts != nil {
		w = &WaitOptions{Timeout: opts.Timeout}
	}
	return c.WaitForCrawl(ctx, id, w)
}

// MapOptions are the fields of a map request.
type MapOptions struct {
	Search   string
	Limit    int
	MaxDepth int
}

// Map discovers a site's URLs. opts may be nil.
func (c *Client) Map(ctx context.Context, siteURL string, opts *MapOptions, call ...CallOption) ([]string, error) {
	base := Map{"url": siteURL}
	if opts != nil {
		if opts.Search != "" {
			base["search"] = opts.Search
		}
		if opts.Limit != 0 {
			base["limit"] = opts.Limit
		}
		if opts.MaxDepth != 0 {
			base["max_depth"] = opts.MaxDepth
		}
	}
	path, err := c.org(ctx, "/map")
	if err != nil {
		return nil, err
	}
	var out struct {
		URLs []string `json:"urls"`
	}
	if err := c.post(ctx, path, base, &out, call); err != nil {
		return nil, err
	}
	return out.URLs, nil
}

// --- discovery -----------------------------------------------------------------

// Search runs a web search. options are the API's fields: sources, limit,
// country, freshness, include_domains, exclude_domains, search_depth
// ("advanced" also reads the top results and attaches query-relevant
// passages), auto_parameters, include_answer, scrape. options may be nil.
func (c *Client) Search(ctx context.Context, query string, options Map, call ...CallOption) (*SearchResponse, error) {
	path, err := c.org(ctx, "/search")
	if err != nil {
		return nil, err
	}
	var out SearchResponse
	if err := c.post(ctx, path, payload(Map{"query": query}, options), &out, call); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResearchOptions are the fields of a research request.
type ResearchOptions struct {
	// Depth is "quick" (seconds, the default) or "deep" (1-2 minutes, planned sub-queries).
	Depth string
	// Output is "answer" or "report".
	Output string
	// Schema requests a structured answer (JSON Schema).
	Schema Map
	// NoCache bypasses the short-lived result cache.
	NoCache bool
	Extra   Map
}

// Research answers a question from the web with verified citations. opts may be nil.
func (c *Client) Research(ctx context.Context, query string, opts *ResearchOptions, call ...CallOption) (*ResearchResult, error) {
	base := Map{"query": query, "depth": "quick"}
	if opts != nil {
		if opts.Depth != "" {
			base["depth"] = opts.Depth
		}
		if opts.Output != "" {
			base["output"] = opts.Output
		}
		if opts.Schema != nil {
			base["schema"] = opts.Schema
		}
		if opts.NoCache {
			base["no_cache"] = true
		}
	}
	var extra Map
	if opts != nil {
		extra = opts.Extra
	}
	path, err := c.org(ctx, "/research")
	if err != nil {
		return nil, err
	}
	var out ResearchResult
	if err := c.post(ctx, path, payload(base, extra), &out, call); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExtractOptions are the fields of an extract request.
type ExtractOptions struct {
	Prompt string
	Schema Map
	Extra  Map
}

// Extract extracts structured data across up to 10 URLs. opts may be nil.
func (c *Client) Extract(ctx context.Context, urls []string, opts *ExtractOptions, call ...CallOption) (Map, error) {
	base := Map{"urls": urls}
	var extra Map
	if opts != nil {
		if opts.Prompt != "" {
			base["prompt"] = opts.Prompt
		}
		if opts.Schema != nil {
			base["schema"] = opts.Schema
		}
		extra = opts.Extra
	}
	path, err := c.org(ctx, "/extract")
	if err != nil {
		return nil, err
	}
	var out Map
	if err := c.post(ctx, path, payload(base, extra), &out, call); err != nil {
		return nil, err
	}
	return out, nil
}

// LlmsTxtOptions are the fields of an llms.txt request.
type LlmsTxtOptions struct {
	MaxURLs  int
	FullText bool
}

// LlmsTxt generates llms.txt for a site. opts may be nil.
func (c *Client) LlmsTxt(ctx context.Context, siteURL string, opts *LlmsTxtOptions, call ...CallOption) (Map, error) {
	base := Map{"url": siteURL}
	if opts != nil {
		if opts.MaxURLs != 0 {
			base["max_urls"] = opts.MaxURLs
		}
		if opts.FullText {
			base["full_text"] = true
		}
	}
	path, err := c.org(ctx, "/llmstxt")
	if err != nil {
		return nil, err
	}
	var out Map
	if err := c.post(ctx, path, base, &out, call); err != nil {
		return nil, err
	}
	return out, nil
}
