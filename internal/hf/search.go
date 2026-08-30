package hf

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Search and model metadata (DESIGN sections 7.1, 3.6).
//
//	GET {endpoint}/api/models?filter=gguf&search=&sort=downloads&direction=-1&limit=30&cursor=
//	GET {endpoint}/api/models/{repo}
//
// `filter=gguf` is not a convenience. This product runs llama.cpp and nothing
// else, and a search that returned safetensors repositories would offer the user
// a model it cannot run — the whole of SPEC section 3.2's "one click from search
// to running" turns on the list being a list of things that work.

// DefaultSearchLimit is section 7.1's page size.
const DefaultSearchLimit = 30

// MaxSearchLimit bounds what a client may ask for in one page. The Hub itself
// caps a listing; this is the bound this daemon will forward.
const MaxSearchLimit = 100

// SearchParams is `GET /api/v1/hf/search`'s query, normalized.
type SearchParams struct {
	// Query is the free-text `search=`. Empty lists the most-downloaded GGUF
	// repositories, which is what an empty search box should show.
	Query string
	// Author restricts to one namespace (`?author=`).
	Author string
	// Sort is `downloads`, `likes`, `lastModified` or `trendingScore`. Empty
	// uses `downloads`, which is the order a user without an opinion wants.
	Sort string
	// Limit is the page size; zero uses DefaultSearchLimit.
	Limit int
	// Cursor is the Hub's own opaque pagination cursor, passed through
	// unmodified. It is never interpreted here: it belongs to the Hub, and a
	// client that parsed it would break the first time the Hub changed it.
	Cursor string
}

// SearchResult is one normalized row of a search.
//
// It is a projection rather than the Hub's raw JSON: the Hub returns dozens of
// fields, its shape changes without notice, and section 3.6 fixes exactly what
// `GET /api/v1/hf/search` promises. A field this struct does not carry is a
// field the UI cannot come to depend on.
type SearchResult struct {
	ID        string   `json:"id"`
	Author    string   `json:"author"`
	Downloads int64    `json:"downloads"`
	Likes     int64    `json:"likes"`
	Gated     bool     `json:"gated"`
	Private   bool     `json:"private"`
	UpdatedAt *string  `json:"updated_at"`
	Tags      []string `json:"tags"`
	// GGUF is the Hub's own server-computed summary, present on repositories it
	// has indexed. It is nil when the Hub did not compute one, which is a fact
	// and not a zero: `context_length: 0` would read as "this model has no
	// context", and F14 forbids exactly that.
	GGUF *GGUFSummary `json:"gguf"`
}

// GGUFSummary is the Hub's `gguf` object: architecture, trained context length
// and totals, computed server-side from the repository's files.
//
// It is a courtesy, never the authority. Section 8's fit calculator reads the
// header this daemon parsed itself — from a local file, or over HTTP Range
// through Peek — because the Hub's summary describes "the repository" while a
// fit answer is about one quantization of one shard set.
type GGUFSummary struct {
	Architecture  string `json:"architecture,omitempty"`
	ContextLength int64  `json:"context_length,omitempty"`
	Total         int64  `json:"total,omitempty"`
	BOSToken      string `json:"bos_token,omitempty"`
	EOSToken      string `json:"eos_token,omitempty"`
}

// SearchPage is one page of results plus the cursor for the next.
type SearchPage struct {
	Items []SearchResult
	// NextCursor is empty on the last page. It is the Hub's cursor verbatim.
	NextCursor string
}

// Search lists GGUF repositories.
func (c *Client) Search(ctx context.Context, p SearchParams) (SearchPage, error) {
	q := url.Values{}
	q.Set("filter", "gguf")
	if p.Query != "" {
		q.Set("search", p.Query)
	}
	if p.Author != "" {
		q.Set("author", p.Author)
	}
	sort := p.Sort
	if sort == "" {
		sort = "downloads"
	}
	q.Set("sort", sort)
	q.Set("direction", "-1")
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	q.Set("limit", strconv.Itoa(limit))
	if p.Cursor != "" {
		q.Set("cursor", p.Cursor)
	}
	// `full=false` keeps the payload to the fields below; `config=false` keeps
	// the Hub from computing a config object nothing here reads.
	q.Set("full", "false")

	raw := c.endpoint + "/api/models?" + q.Encode()

	resp, err := c.do(ctx, request{
		method: "GET", url: raw, cacheKey: "search:" + q.Encode(),
		header: jsonAccept(),
	})
	if err != nil {
		return SearchPage{}, err
	}

	var rows []hubModel
	if err := json.Unmarshal(resp.body, &rows); err != nil {
		return SearchPage{}, fmt.Errorf("hf: decoding the search response: %w", err)
	}
	out := SearchPage{Items: make([]SearchResult, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, r.searchResult())
	}
	// A cached page has no response headers to read a Link from, and that is
	// correct rather than lossy: the cursor was already delivered with the live
	// answer, and the client is paging with a cursor it holds.
	if resp.header != nil {
		out.NextCursor = nextCursorFromLink(resp.header.Get("Link"))
	}
	return out, nil
}

// ModelInfo is `GET {endpoint}/api/models/{repo}` — the metadata section 3.6's
// model page is built from.
type ModelInfo struct {
	ID string `json:"id"`
	// SHA is the resolved commit of the default branch. It is what makes a
	// download reproducible: `models.revision` is a commit and never `main`,
	// because `main` names a different tree next week.
	SHA          string       `json:"sha"`
	Author       string       `json:"author"`
	Gated        bool         `json:"gated"`
	Private      bool         `json:"private"`
	Disabled     bool         `json:"disabled"`
	Downloads    int64        `json:"downloads"`
	Likes        int64        `json:"likes"`
	LastModified *string      `json:"last_modified"`
	Tags         []string     `json:"tags"`
	Siblings     []string     `json:"siblings"`
	GGUF         *GGUFSummary `json:"gguf"`
	// CardData is the model card's YAML front matter as the Hub parsed it —
	// license, base model, pipeline tag. It is passed through rather than
	// modeled: it is display material, and the set of keys is the community's.
	CardData map[string]any `json:"card_data,omitempty"`
}

// Model fetches one repository's metadata.
func (c *Client) Model(ctx context.Context, repo string) (ModelInfo, error) {
	if err := validateRepo(repo); err != nil {
		return ModelInfo{}, err
	}
	raw := c.endpoint + "/api/models/" + repo

	body, err := c.getJSON(ctx, "model:"+repo, raw, repo)
	if err != nil {
		return ModelInfo{}, err
	}
	var m hubModel
	if err := json.Unmarshal(body, &m); err != nil {
		return ModelInfo{}, fmt.Errorf("hf: decoding the model response: %w", err)
	}
	return m.modelInfo(), nil
}

// hubModel is the Hub's JSON, decoded loosely.
//
// `gated` is the field that forces this to be its own type: the Hub sends
// `false` for an open repository and the STRING `"auto"` or `"manual"` for a
// gated one, so a `bool` field would fail to unmarshal on exactly the
// repositories this product most needs to recognize.
type hubModel struct {
	ID           string          `json:"id"`
	ModelID      string          `json:"modelId"`
	SHA          string          `json:"sha"`
	Author       string          `json:"author"`
	Gated        json.RawMessage `json:"gated"`
	Private      bool            `json:"private"`
	Disabled     bool            `json:"disabled"`
	Downloads    int64           `json:"downloads"`
	Likes        int64           `json:"likes"`
	LastModified string          `json:"lastModified"`
	Tags         []string        `json:"tags"`
	Siblings     []struct {
		Filename string `json:"rfilename"`
	} `json:"siblings"`
	GGUF     *GGUFSummary   `json:"gguf"`
	CardData map[string]any `json:"cardData"`
}

func (m hubModel) id() string {
	if m.ID != "" {
		return m.ID
	}
	return m.ModelID
}

// gated decodes the tri-state field. Anything that is not literal `false`,
// `null` or an empty string is a gate.
func (m hubModel) gated() bool {
	s := strings.TrimSpace(string(m.Gated))
	switch s {
	case "", "false", "null", `""`:
		return false
	default:
		return true
	}
}

func (m hubModel) author() string {
	if m.Author != "" {
		return m.Author
	}
	if org, _, ok := strings.Cut(m.id(), "/"); ok {
		return org
	}
	return ""
}

func (m hubModel) updatedAt() *string {
	if m.LastModified == "" {
		return nil
	}
	// The Hub sends RFC 3339 already; normalizing through time keeps the wire
	// form section 3 promises (UTC, no fractional seconds) even if it changes.
	if t, err := time.Parse(time.RFC3339, m.LastModified); err == nil {
		s := t.UTC().Format(time.RFC3339)
		return &s
	}
	s := m.LastModified
	return &s
}

func (m hubModel) searchResult() SearchResult {
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	return SearchResult{
		ID:        m.id(),
		Author:    m.author(),
		Downloads: m.Downloads,
		Likes:     m.Likes,
		Gated:     m.gated(),
		Private:   m.Private,
		UpdatedAt: m.updatedAt(),
		Tags:      tags,
		GGUF:      m.GGUF,
	}
}

func (m hubModel) modelInfo() ModelInfo {
	siblings := make([]string, 0, len(m.Siblings))
	for _, s := range m.Siblings {
		siblings = append(siblings, s.Filename)
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	return ModelInfo{
		ID:           m.id(),
		SHA:          m.SHA,
		Author:       m.author(),
		Gated:        m.gated(),
		Private:      m.Private,
		Disabled:     m.Disabled,
		Downloads:    m.Downloads,
		Likes:        m.Likes,
		LastModified: m.updatedAt(),
		Tags:         tags,
		Siblings:     siblings,
		GGUF:         m.GGUF,
		CardData:     m.CardData,
	}
}

func jsonAccept() map[string][]string {
	return map[string][]string{"Accept": {"application/json"}}
}

// nextCursorFromLink reads the Hub's `Link: <…?cursor=X>; rel="next"` header.
// The cursor is extracted and passed back to the client opaquely; the URL it
// came in is discarded, because a client that followed a Hub URL directly would
// bypass this daemon's own auth.
func nextCursorFromLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end <= start {
			continue
		}
		u, err := url.Parse(part[start+1 : end])
		if err != nil {
			continue
		}
		return u.Query().Get("cursor")
	}
	return ""
}
