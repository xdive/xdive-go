package xdive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rec struct {
	method, path string
	header       http.Header
	body         map[string]any
}

// server starts a test server; handler gets the 1-based request count.
func server(t *testing.T, h func(n int, w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]rec) {
	t.Helper()
	var mu sync.Mutex
	var recs []rec
	var n int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		recs = append(recs, rec{r.Method, r.URL.Path, r.Header.Clone(), body})
		mu.Unlock()
		h(int(atomic.AddInt32(&n, 1)), w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, &recs
}

func jsonResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newClient(t *testing.T, ts *httptest.Server, opts ...Option) *Client {
	t.Helper()
	all := append([]Option{WithAPIKey("ck_test"), WithBaseURL(ts.URL), WithOrgID("org1")}, opts...)
	c, err := New(all...)
	if err != nil {
		t.Fatal(err)
	}
	c.backoffBase = time.Millisecond
	return c
}

func TestNewRequiresKeyAndReadsEnv(t *testing.T) {
	t.Setenv("XDIVE_API_KEY", "")
	t.Setenv("CRAWLER_API_KEY", "")
	if _, err := New(); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("want ErrNoAPIKey, got %v", err)
	}
	t.Setenv("CRAWLER_API_KEY", "legacy")
	t.Setenv("CRAWLER_BASE_URL", "http://legacy.test/api/")
	c, err := New()
	if err != nil || c.apiKey != "legacy" || c.BaseURL() != "http://legacy.test/api" {
		t.Fatalf("fallback env: %v %+v", err, c)
	}
	t.Setenv("XDIVE_API_KEY", "new")
	t.Setenv("XDIVE_BASE_URL", "http://new.test/api")
	c, _ = New()
	if c.apiKey != "new" || c.BaseURL() != "http://new.test/api" {
		t.Fatalf("XDIVE_* must win: %+v", c)
	}
	c, _ = New(WithAPIKey("explicit"), WithBaseURL("http://x.test"))
	if c.apiKey != "explicit" || c.BaseURL() != "http://x.test" {
		t.Fatalf("options must win: %+v", c)
	}
	t.Setenv("XDIVE_BASE_URL", "")
	t.Setenv("CRAWLER_BASE_URL", "")
	c, _ = New()
	if c.BaseURL() != DefaultBaseURL || DefaultBaseURL != "https://xdive.dev/api" {
		t.Fatalf("default base url: %s", c.BaseURL())
	}
}

func TestHeadersAndOrgDiscoveryCached(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/whoami" {
			jsonResp(w, 200, Map{"orgId": "org9"})
			return
		}
		jsonResp(w, 200, Map{"data": Map{"url": "https://e.com", "markdown": "# hi"}})
	})
	c, _ := New(WithAPIKey("ck_test"), WithBaseURL(ts.URL))
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Scrape(context.Background(), "https://e.com", nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	whoamis := 0
	for _, r := range *recs {
		if r.path == "/whoami" {
			whoamis++
			if r.header.Get("X-API-Key") != "ck_test" {
				t.Error("whoami missing key")
			}
		} else {
			if r.path != "/orgs/org9/scrape" {
				t.Errorf("path %s", r.path)
			}
			h := r.header
			if h.Get("X-API-Key") != "ck_test" || h.Get("Accept") != "application/json" ||
				h.Get("User-Agent") != "xdive-go/"+Version || h.Get("Content-Type") != "application/json" {
				t.Errorf("headers %v", h)
			}
		}
	}
	// Concurrent first calls may each discover once, but never after the cache is filled.
	if whoamis < 1 || whoamis > 5 {
		t.Fatalf("whoami calls %d", whoamis)
	}
	before := len(*recs)
	if _, err := c.Scrape(context.Background(), "https://e.com", nil); err != nil {
		t.Fatal(err)
	}
	if len(*recs) != before+1 {
		t.Fatal("org id was not cached")
	}
}

func TestWithOrgIDSkipsWhoami(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		jsonResp(w, 200, Map{"urls": []string{"https://e.com/a"}})
	})
	c := newClient(t, ts)
	urls, err := c.Map(context.Background(), "https://e.com", &MapOptions{Search: "doc", Limit: 5, MaxDepth: 2})
	if err != nil || len(urls) != 1 {
		t.Fatal(err, urls)
	}
	if len(*recs) != 1 || (*recs)[0].path != "/orgs/org1/map" {
		t.Fatalf("%+v", *recs)
	}
	b := (*recs)[0].body
	if b["search"] != "doc" || b["limit"] != float64(5) || b["max_depth"] != float64(2) {
		t.Fatalf("body %v", b)
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestIdempotencyKeyReusedAcrossRetriesAndNewPerCall(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			jsonResp(w, 429, Map{"success": false, "code": "RATE_LIMITED", "error": "slow", "retryable": true})
			return
		}
		jsonResp(w, 200, Map{"data": Map{"url": "u"}})
	})
	c := newClient(t, ts)
	ctx := context.Background()
	if _, err := c.Scrape(ctx, "https://e.com", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Scrape(ctx, "https://e.com", nil); err != nil {
		t.Fatal(err)
	}
	r := *recs
	if len(r) != 3 {
		t.Fatalf("requests %d", len(r))
	}
	k1, k2, k3 := r[0].header.Get("Idempotency-Key"), r[1].header.Get("Idempotency-Key"), r[2].header.Get("Idempotency-Key")
	if !uuidRE.MatchString(k1) {
		t.Fatalf("not a v4 uuid: %q", k1)
	}
	if k1 != k2 {
		t.Fatal("retry must reuse the key")
	}
	if k3 == k1 {
		t.Fatal("new call must get a new key")
	}
}

func TestIdempotencyKeyOverrideAndNoKeyOnGet(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		jsonResp(w, 200, Map{"id": "c1", "status": "running"})
	})
	c := newClient(t, ts)
	if _, err := c.StartCrawl(context.Background(), "https://e.com", nil, WithIdempotencyKey("mine-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetCrawl(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if got := (*recs)[0].header.Get("Idempotency-Key"); got != "mine-1" {
		t.Fatalf("key %q", got)
	}
	if got := (*recs)[1].header.Get("Idempotency-Key"); got != "" {
		t.Fatalf("GET must not carry a key, got %q", got)
	}
}

func TestRetryOnOverloadStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
	}{
		{"429", 429, ""},
		{"503 busy", 503, "SERVER_BUSY"},
		{"409 in progress", 409, "IDEMPOTENCY_KEY_IN_PROGRESS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n <= 2 {
					w.Header().Set("Retry-After", "0")
					jsonResp(w, tc.status, Map{"code": tc.code, "error": "wait"})
					return
				}
				jsonResp(w, 200, Map{"query": "q", "provider": "p", "results": []any{}})
			})
			c := newClient(t, ts)
			if _, err := c.Search(context.Background(), "q", nil); err != nil {
				t.Fatal(err)
			}
			if len(*recs) != 3 {
				t.Fatalf("requests %d", len(*recs))
			}
		})
	}
}

func TestNoRetryOnOtherStatuses(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{500, ""}, {503, "NOT_CONFIGURED"}, {409, "CONFLICT"}, {400, "BAD_REQUEST"}, {404, ""}} {
		ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
			jsonResp(w, tc.status, Map{"code": tc.code, "error": "nope"})
		})
		c := newClient(t, ts)
		_, err := c.Search(context.Background(), "q", nil)
		var e *Error
		if !errors.As(err, &e) || e.Status != tc.status {
			t.Fatalf("status %d: %v", tc.status, err)
		}
		if len(*recs) != 1 {
			t.Fatalf("status %d retried %d times", tc.status, len(*recs))
		}
	}
}

func TestRateLimitedErrorAfterMaxRetries(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.Header().Set("X-Request-ID", "req-7")
		jsonResp(w, 429, Map{"success": false, "code": "RATE_LIMITED", "error": "too many", "retryable": false, "docs_url": "https://docs/rl"})
	})
	c := newClient(t, ts, WithMaxRetries(2))
	_, err := c.Search(context.Background(), "q", nil)
	if len(*recs) != 3 {
		t.Fatalf("attempts %d", len(*recs))
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited: %v", err)
	}
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 0 {
		t.Fatalf("RateLimitedError: %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Status != 429 || e.Code != "RATE_LIMITED" || e.RequestID != "req-7" ||
		e.DocsURL != "https://docs/rl" || !e.Retryable {
		t.Fatalf("Error: %+v", e)
	}
}

func TestRetryAfterHonored(t *testing.T) {
	ts, _ := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			w.Header().Set("Retry-After", "0.2")
			jsonResp(w, 429, Map{"error": "x"})
			return
		}
		jsonResp(w, 200, Map{"query": "q"})
	})
	c := newClient(t, ts)
	start := time.Now()
	if _, err := c.Search(context.Background(), "q", nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 180*time.Millisecond {
		t.Fatalf("did not wait for Retry-After: %v", d)
	}
}

func TestErrorFieldsAndLegacyBody(t *testing.T) {
	ts, _ := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orgs/org1/search" {
			w.Header().Set("X-Request-ID", "rid")
			jsonResp(w, 402, Map{"success": false, "code": "QUOTA_EXCEEDED", "error": "out of credits", "retryable": false, "docs_url": "https://docs/q"})
			return
		}
		jsonResp(w, 400, Map{"error": "bad url"})
	})
	c := newClient(t, ts)
	_, err := c.Search(context.Background(), "q", nil)
	var e *Error
	if !errors.As(err, &e) || e.Status != 402 || e.Code != "QUOTA_EXCEEDED" || e.Message != "out of credits" ||
		e.RequestID != "rid" || e.DocsURL != "https://docs/q" || e.Retryable {
		t.Fatalf("%+v", e)
	}
	if !errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrRateLimited) {
		t.Fatal("sentinel matching")
	}
	if !strings.Contains(err.Error(), "402 QUOTA_EXCEEDED: out of credits") {
		t.Fatal(err.Error())
	}
	_, err = c.Scrape(context.Background(), "x", nil)
	if !errors.As(err, &e) || e.Status != 400 || e.Code != "" || e.Message != "bad url" {
		t.Fatalf("legacy: %+v", e)
	}
}

func TestNonJSONErrorBody(t *testing.T) {
	ts, _ := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	})
	c := newClient(t, ts)
	_, err := c.Search(context.Background(), "q", nil)
	var e *Error
	if !errors.As(err, &e) || e.Status != 502 || e.Message != "Bad Gateway" {
		t.Fatalf("%v", err)
	}
}

func TestTransportErrorRetriedForGetAndPostNotDelete(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 && r.Method != http.MethodDelete {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		if r.Method == http.MethodDelete {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		jsonResp(w, 200, Map{"query": "q"})
	}))
	defer ts.Close()
	c := newClient(t, ts)
	if _, err := c.Search(context.Background(), "q", nil); err != nil {
		t.Fatalf("POST should survive dropped connections: %v", err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("calls %d", atomic.LoadInt32(&calls))
	}
	atomic.StoreInt32(&calls, 100)
	if err := c.CancelCrawl(context.Background(), "c1"); err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&calls) != 101 {
		t.Fatalf("DELETE must not be retried, calls %d", atomic.LoadInt32(&calls))
	}
}

func TestTimeoutIsRetried(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			time.Sleep(200 * time.Millisecond)
			return
		}
		jsonResp(w, 200, Map{"query": "q"})
	})
	c := newClient(t, ts, WithTimeout(50*time.Millisecond))
	if _, err := c.Search(context.Background(), "q", nil); err != nil {
		t.Fatal(err)
	}
	if len(*recs) != 2 || (*recs)[0].header.Get("Idempotency-Key") != (*recs)[1].header.Get("Idempotency-Key") {
		t.Fatal("timeout retry must reuse the key")
	}
}

func TestTransportErrorGivesUpAfterMaxRetries(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer ts.Close()
	c := newClient(t, ts, WithMaxRetries(1))
	if _, err := c.Search(context.Background(), "q", nil); err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("calls %d", atomic.LoadInt32(&calls))
	}
}

func TestBackoffSchedule(t *testing.T) {
	c := &Client{backoffBase: time.Second}
	want := []time.Duration{1, 2, 4, 8, 8, 8}
	for i, w := range want {
		if got := c.backoff(i); got != w*time.Second {
			t.Errorf("attempt %d: %v", i, got)
		}
	}
	if got := c.backoff(200); got != 8*time.Second {
		t.Errorf("overflow guard: %v", got)
	}
}

func TestContextCancelDuringRetrySleep(t *testing.T) {
	ts, _ := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		jsonResp(w, 429, Map{"error": "x"})
	})
	c := newClient(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Search(ctx, "q", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("sleep ignored the context")
	}
}

func TestContextCancelledBeforeCall(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) { jsonResp(w, 200, Map{}) })
	c := newClient(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Search(ctx, "q", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(*recs) != 0 {
		t.Fatal("request sent with cancelled context")
	}
}

func TestScrapeFormatsAndExtra(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		jsonResp(w, 200, Map{"data": Map{
			"url":       "https://e.com",
			"title":     "T",
			"artifacts": Map{"markdown": Map{"content": "# artifact"}},
			"extra":     1,
		}})
	})
	c := newClient(t, ts)
	page, err := c.Scrape(context.Background(), "https://e.com", &ScrapeOptions{
		Formats: append(F("markdown"), Format{Type: "json", Prompt: "p", Schema: Map{"type": "object"}, MaxResults: 3}),
		Extra:   Map{"only_main_content": true, "nilval": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if PageMarkdown(page) != "# artifact" || page.Title != "T" || !strings.Contains(string(page.Raw), `"extra"`) {
		t.Fatalf("%+v", page)
	}
	b := (*recs)[0].body
	fs := b["formats"].([]any)
	if fs[0].(map[string]any)["type"] != "markdown" {
		t.Fatalf("%v", fs)
	}
	j := fs[1].(map[string]any)
	if j["type"] != "json" || j["prompt"] != "p" || j["max_results"] != float64(3) || j["schema"] == nil {
		t.Fatalf("%v", j)
	}
	if b["only_main_content"] != true || b["url"] != "https://e.com" {
		t.Fatalf("%v", b)
	}
	if _, ok := b["nilval"]; ok {
		t.Fatal("nil extras must be dropped")
	}
}

func TestPageMarkdownFallback(t *testing.T) {
	if PageMarkdown(nil) != "" {
		t.Fatal("nil")
	}
	if PageMarkdown(&Page{Markdown: "m"}) != "m" {
		t.Fatal("fallback")
	}
	if PageMarkdown(&Page{Markdown: "m", Artifacts: map[string]Artifact{"markdown": {Content: "a"}}}) != "a" {
		t.Fatal("artifact first")
	}
}

func TestCrawlStartGetCancelErrors(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			jsonResp(w, 202, Map{"id": "c/1"})
		case r.Method == "DELETE":
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/errors"):
			jsonResp(w, 200, Map{"id": "c1", "status": "success", "errors": []Map{{"url": "u", "error": "boom"}}, "robots_blocked": []string{"r"}})
		default:
			jsonResp(w, 200, Map{"id": "c1", "status": "success", "pages": 2, "results": []Map{{"url": "a"}, {"url": "b"}}})
		}
	})
	c := newClient(t, ts)
	ctx := context.Background()
	id, err := c.StartCrawl(ctx, "https://e.com", &CrawlOptions{Formats: F("markdown"), Extra: Map{"limit": 10}})
	if err != nil || id != "c/1" {
		t.Fatal(id, err)
	}
	if (*recs)[0].path != "/orgs/org1/crawl" || (*recs)[0].body["limit"] != float64(10) {
		t.Fatalf("%+v", (*recs)[0])
	}
	r, err := c.GetCrawl(ctx, "c1")
	if err != nil || !r.Done() || r.Pages != 2 || len(r.Results) != 2 {
		t.Fatal(r, err)
	}
	if err := c.CancelCrawl(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	ce, err := c.CrawlErrors(ctx, "c1")
	if err != nil || len(ce.Errors) != 1 || ce.Errors[0].Error != "boom" || ce.RobotsBlocked[0] != "r" {
		t.Fatal(ce, err)
	}
	if rr := *recs; rr[2].method != "DELETE" || rr[2].path != "/orgs/org1/crawls/c1" || rr[3].path != "/orgs/org1/crawls/c1/errors" {
		t.Fatalf("%+v", rr)
	}
}

func TestWaitForCrawlETagAndRetryAfter(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		switch n {
		case 1:
			w.Header().Set("ETag", `"v1"`)
			jsonResp(w, 200, Map{"id": "c1", "status": "running"})
		case 2:
			if r.Header.Get("If-None-Match") != `"v1"` {
				t.Errorf("If-None-Match = %q", r.Header.Get("If-None-Match"))
			}
			w.WriteHeader(304)
		default:
			w.Header().Set("ETag", `"v2"`)
			jsonResp(w, 200, Map{"id": "c1", "status": "success", "results": []Map{{"url": "a"}}})
		}
	})
	c := newClient(t, ts)
	rec, err := c.WaitForCrawl(context.Background(), "c1", &WaitOptions{PollInterval: time.Millisecond})
	if err != nil || rec.Status != "success" || len(rec.Results) != 1 {
		t.Fatal(rec, err)
	}
	if len(*recs) != 3 || (*recs)[2].header.Get("If-None-Match") != `"v1"` {
		t.Fatalf("%d requests", len(*recs))
	}
}

func TestWaitForCrawlTimeout(t *testing.T) {
	ts, _ := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		jsonResp(w, 200, Map{"id": "c1", "status": "running"})
	})
	c := newClient(t, ts)
	start := time.Now()
	_, err := c.WaitForCrawl(context.Background(), "c1", &WaitOptions{Timeout: 150 * time.Millisecond, PollInterval: 20 * time.Millisecond})
	if !errors.Is(err, ErrCrawlTimeout) {
		t.Fatalf("want ErrCrawlTimeout: %v", err)
	}
	var ct *CrawlTimeoutError
	var e *Error
	if !errors.As(err, &ct) || ct.CrawlID != "c1" || !errors.As(err, &e) || e.Status != 408 {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("too slow")
	}
}

func TestWaitForCrawlContextCancel(t *testing.T) {
	ts, _ := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		jsonResp(w, 200, Map{"id": "c1", "status": "running"})
	})
	c := newClient(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.WaitForCrawl(ctx, "c1", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestCrawlWaits(t *testing.T) {
	ts, _ := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			jsonResp(w, 202, Map{"id": "c1"})
			return
		}
		jsonResp(w, 200, Map{"id": "c1", "status": "error", "error": "failed"})
	})
	c := newClient(t, ts)
	rec, err := c.Crawl(context.Background(), "https://e.com", nil)
	if err != nil || rec.Status != "error" || rec.Error != "failed" {
		t.Fatal(rec, err)
	}
}

func TestBatchScrapeWaitAndNoWait(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			jsonResp(w, 202, Map{"id": "b1"})
			return
		}
		jsonResp(w, 200, Map{"id": "b1", "status": "success"})
	})
	c := newClient(t, ts)
	ctx := context.Background()
	id, err := c.StartBatchScrape(ctx, []string{"a", "b"}, &BatchOptions{Formats: F("markdown")})
	if err != nil || id != "b1" || len(*recs) != 1 {
		t.Fatal(id, err, len(*recs))
	}
	if (*recs)[0].path != "/orgs/org1/batch/scrape" {
		t.Fatal((*recs)[0].path)
	}
	rec, err := c.BatchScrape(ctx, []string{"a"}, nil)
	if err != nil || rec.Status != "success" {
		t.Fatal(rec, err)
	}
}

func TestSearchResearchExtractLlmsTxt(t *testing.T) {
	ts, recs := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			jsonResp(w, 200, Map{"query": "q", "provider": "p", "results": []Map{{
				"id": "1", "source": "web", "rank": 1, "title": "T", "url": "https://t",
				"passages": []Map{{"text": "x", "score": 0.5, "start": 3}}, "relevance_score": 0.9,
			}}, "answer": Map{"text": "ans", "citations": []Map{{"number": 1, "url": "https://t"}}}})
		case strings.HasSuffix(r.URL.Path, "/research"):
			jsonResp(w, 200, Map{"query": "q", "answer": "A [1]", "confidence": "high",
				"citations": []Map{{"number": 1, "url": "https://t", "title": "T", "quote": "Q"}},
				"sources":   []Map{{"number": 1, "url": "https://t", "title": "T", "passages": []any{}, "score": 1}},
				"queries":   []string{"q"}, "usage": Map{"searches": 1, "pages_read": 2, "rounds": 1, "plan": "single", "duration_ms": 900},
				"cached": true, "data": Map{"k": "v"}})
		case strings.HasSuffix(r.URL.Path, "/extract"):
			jsonResp(w, 200, Map{"data": Map{"price": 1}})
		default:
			jsonResp(w, 200, Map{"llms_txt": "# site"})
		}
	})
	c := newClient(t, ts)
	ctx := context.Background()
	s, err := c.Search(ctx, "q", Map{"limit": 3, "include_answer": true})
	if err != nil || s.Results[0].Passages[0].Start != 3 || s.Answer.Citations[0].Number != 1 || s.Results[0].RelevanceScore != 0.9 {
		t.Fatalf("%+v %v", s, err)
	}
	res, err := c.Research(ctx, "q", &ResearchOptions{Depth: "deep", NoCache: true})
	if err != nil || res.Confidence != "high" || res.Citations[0].Quote != "Q" || res.Usage.PagesRead != 2 || !res.Cached || string(res.Data) != `{"k":"v"}` {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := c.Research(ctx, "q", nil); err != nil {
		t.Fatal(err)
	}
	ex, err := c.Extract(ctx, []string{"https://a"}, &ExtractOptions{Prompt: "p", Schema: Map{"type": "object"}})
	if err != nil || ex["data"] == nil {
		t.Fatal(ex, err)
	}
	lt, err := c.LlmsTxt(ctx, "https://e.com", &LlmsTxtOptions{MaxURLs: 5, FullText: true})
	if err != nil || lt["llms_txt"] != "# site" {
		t.Fatal(lt, err)
	}
	r := *recs
	if r[0].body["limit"] != float64(3) || r[0].body["include_answer"] != true {
		t.Fatalf("%v", r[0].body)
	}
	if r[1].body["depth"] != "deep" || r[1].body["no_cache"] != true {
		t.Fatalf("%v", r[1].body)
	}
	if r[2].body["depth"] != "quick" {
		t.Fatalf("default depth: %v", r[2].body)
	}
	if r[3].body["prompt"] != "p" || r[4].body["max_urls"] != float64(5) || r[4].body["full_text"] != true {
		t.Fatalf("%v %v", r[3].body, r[4].body)
	}
}

func TestWhoamiAndOrgIDErrors(t *testing.T) {
	ts, _ := server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		jsonResp(w, 401, Map{"code": "UNAUTHORIZED", "error": "bad key"})
	})
	c, _ := New(WithAPIKey("k"), WithBaseURL(ts.URL))
	c.backoffBase = time.Millisecond
	if _, err := c.Scrape(context.Background(), "x", nil); err == nil {
		t.Fatal("expected auth error")
	}
	var e *Error
	_, err := c.Whoami(context.Background())
	if !errors.As(err, &e) || e.Status != 401 {
		t.Fatal(err)
	}
}

func TestUUIDv4Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		k := newIdempotencyKey()
		if !uuidRE.MatchString(k) || seen[k] {
			t.Fatalf("bad or duplicate key %q", k)
		}
		seen[k] = true
	}
}

func TestFormatMarshalPlain(t *testing.T) {
	b, _ := json.Marshal(F("markdown", "links"))
	if string(b) != `[{"type":"markdown"},{"type":"links"}]` {
		t.Fatal(string(b))
	}
}
