package xdive

import "encoding/json"

// Map is an open-ended JSON object.
type Map = map[string]any

// FormatName is a canonical output format name.
type FormatName = string

// Canonical format names. Audio and screenshot are available on synchronous
// scrapes only; "raw_html" is an alias of "rawHtml".
const (
	FormatMarkdown   = "markdown"
	FormatText       = "text"
	FormatHTML       = "html"
	FormatRawHTML    = "rawHtml"
	FormatLinks      = "links"
	FormatImages     = "images"
	FormatJSON       = "json"
	FormatSummary    = "summary"
	FormatQuestion   = "question"
	FormatHighlights = "highlights"
	FormatBranding   = "branding"
	FormatAudio      = "audio"
	FormatScreenshot = "screenshot"
	FormatProduct    = "product"
	FormatMenu       = "menu"
)

// Format selects an output format and optionally its options. A Format with
// only Type set serializes like the plain name.
type Format struct {
	Type FormatName
	// Prompt gives instructions to the LLM formats (json, summary, question, ...).
	Prompt string
	// Schema is the JSON Schema for the json format.
	Schema Map
	// Question is the question for the question format (or use Prompt).
	Question string
	Query    string
	// MaxResults is 0 to 50; zero leaves it unset.
	MaxResults int
	// Extra holds any other format options.
	Extra Map
}

// F returns Formats for plain names: F("markdown", "links").
func F(names ...FormatName) []Format {
	out := make([]Format, len(names))
	for i, n := range names {
		out[i] = Format{Type: n}
	}
	return out
}

// MarshalJSON encodes the format as the API's {"type": ...} object.
func (f Format) MarshalJSON() ([]byte, error) {
	m := Map{"type": f.Type}
	if f.Prompt != "" {
		m["prompt"] = f.Prompt
	}
	if f.Schema != nil {
		m["schema"] = f.Schema
	}
	if f.Question != "" {
		m["question"] = f.Question
	}
	if f.Query != "" {
		m["query"] = f.Query
	}
	if f.MaxResults != 0 {
		m["max_results"] = f.MaxResults
	}
	for k, v := range f.Extra {
		m[k] = v
	}
	return json.Marshal(m)
}

// Artifact is one rendered format of a page.
type Artifact struct {
	Content string `json:"content,omitempty"`
}

// Page is a scraped page. Fields not modeled here are available in Raw.
type Page struct {
	URL         string              `json:"url"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Markdown    string              `json:"markdown,omitempty"`
	Artifacts   map[string]Artifact `json:"artifacts,omitempty"`
	Error       string              `json:"error,omitempty"`
	// Raw is the complete JSON object as received.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the page and keeps the raw object.
func (p *Page) UnmarshalJSON(b []byte) error {
	type plain Page
	var v plain
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*p = Page(v)
	p.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// PageMarkdown returns the Markdown content of a page, whichever field
// carries it. A nil page yields "".
func PageMarkdown(p *Page) string {
	if p == nil {
		return ""
	}
	if a, ok := p.Artifacts["markdown"]; ok && a.Content != "" {
		return a.Content
	}
	return p.Markdown
}

// Crawl statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSuccess   = "success"
	StatusError     = "error"
	StatusCancelled = "cancelled"
)

// CrawlRecord is the state of a crawl or batch scrape job.
type CrawlRecord struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Pages   int    `json:"pages,omitempty"`
	Error   string `json:"error,omitempty"`
	Results []Page `json:"results,omitempty"`
	// Raw is the complete JSON object as received.
	Raw json.RawMessage `json:"-"`
}

// Done reports whether the job reached a terminal status.
func (c *CrawlRecord) Done() bool { return isTerminal(c.Status) }

// UnmarshalJSON decodes the record and keeps the raw object.
func (c *CrawlRecord) UnmarshalJSON(b []byte) error {
	type plain CrawlRecord
	var v plain
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*c = CrawlRecord(v)
	c.Raw = append(json.RawMessage(nil), b...)
	return nil
}

func isTerminal(status string) bool {
	return status == StatusSuccess || status == StatusError || status == StatusCancelled
}

// PageError is one page that failed during a crawl.
type PageError struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// CrawlErrors lists the failed pages of a crawl or batch scrape.
type CrawlErrors struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Error is the crawl-level error, empty when the crawl itself did not fail.
	Error         string      `json:"error,omitempty"`
	Errors        []PageError `json:"errors"`
	RobotsBlocked []string    `json:"robots_blocked"`
}

// Passage is a query-relevant excerpt of a page.
type Passage struct {
	Text  string  `json:"text"`
	Score float64 `json:"score"`
	Start int     `json:"start"`
}

// Citation points an answer at a source.
type Citation struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title,omitempty"`
	Quote  string `json:"quote,omitempty"`
}

// SearchResult is one search hit.
type SearchResult struct {
	ID             string    `json:"id"`
	Source         string    `json:"source"`
	Rank           int       `json:"rank"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	Description    string    `json:"description,omitempty"`
	PublishedAt    string    `json:"published_at,omitempty"`
	Page           *Page     `json:"page,omitempty"`
	Passages       []Passage `json:"passages,omitempty"`
	RelevanceScore float64   `json:"relevance_score,omitempty"`
}

// SearchAnswer is the optional LLM answer of a search.
type SearchAnswer struct {
	Text       string     `json:"text,omitempty"`
	Citations  []Citation `json:"citations,omitempty"`
	Confidence string     `json:"confidence,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// SearchResponse is the result of Search.
type SearchResponse struct {
	Query      string         `json:"query"`
	Provider   string         `json:"provider"`
	Results    []SearchResult `json:"results"`
	Answer     *SearchAnswer  `json:"answer,omitempty"`
	Parameters Map            `json:"parameters,omitempty"`
}

// ResearchSource is a source read during research.
type ResearchSource struct {
	Number   int       `json:"number"`
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Passages []Passage `json:"passages"`
	Score    float64   `json:"score"`
	Snippet  bool      `json:"snippet,omitempty"`
}

// ResearchUsage reports the work done for one research call.
type ResearchUsage struct {
	Searches  int `json:"searches"`
	PagesRead int `json:"pages_read"`
	Rounds    int `json:"rounds"`
	Hops      int `json:"hops,omitempty"`
	// Plan is "single", "multi_hop" or "unavailable".
	Plan       string `json:"plan,omitempty"`
	DurationMs int    `json:"duration_ms"`
	SearchMs   int    `json:"search_ms,omitempty"`
	ReadMs     int    `json:"read_ms,omitempty"`
	ModelMs    int    `json:"model_ms,omitempty"`
}

// ResearchResult is a cited answer to a question.
type ResearchResult struct {
	Query string `json:"query"`
	// Answer is the text answer (or report).
	Answer string `json:"answer,omitempty"`
	// Data is the structured answer when a schema was given.
	Data      json.RawMessage  `json:"data,omitempty"`
	Citations []Citation       `json:"citations"`
	Sources   []ResearchSource `json:"sources"`
	Queries   []string         `json:"queries"`
	// Confidence is "high", "medium", "low" or "none".
	Confidence string        `json:"confidence"`
	Usage      ResearchUsage `json:"usage"`
	// Cached is true when served from the short-lived result cache.
	Cached bool `json:"cached,omitempty"`
}
