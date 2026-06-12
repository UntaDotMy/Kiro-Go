package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// External web-search providers, ported from 9router's APIKEY_PROVIDERS search
// set (open-sse/config/providers.js). These are alternative backends for the
// proxy-side web_search emulation: when config.GetWebSearchProvider() names one of
// these (instead of the default "kiro" MCP search), the web-search loop calls
// performExternalWebSearch, which dispatches to the right provider here and returns
// the shared []WebSearchResult shape. This lets an operator with no Kiro account
// still service Claude's hosted web_search via a standalone search API key.
//
// Only the search (not webFetch) capability is ported, since the web-search loop
// consumes a query -> results list. Each provider normalizes its own response
// shape into WebSearchResult{Title, URL, Snippet}.

const externalSearchTimeout = 20 * time.Second

// performExternalWebSearch dispatches a query to the configured external provider.
// Returns an error the loop treats as "search unavailable" (it then tells the model
// to answer from training).
func performExternalWebSearch(ctx context.Context, provider, apiKey, query string) ([]WebSearchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, externalSearchTimeout)
	defer cancel()

	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("empty query")
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "tavily":
		return searchTavily(ctx, apiKey, q)
	case "brave", "brave-search":
		return searchBrave(ctx, apiKey, q)
	case "serper":
		return searchSerper(ctx, apiKey, q)
	case "exa":
		return searchExa(ctx, apiKey, q)
	case "linkup":
		return searchLinkup(ctx, apiKey, q)
	case "searchapi":
		return searchSearchAPI(ctx, apiKey, q)
	case "youcom", "you":
		return searchYouCom(ctx, apiKey, q)
	case "google-pse", "google_pse":
		return searchGooglePSE(ctx, apiKey, q)
	default:
		return nil, fmt.Errorf("unknown external web-search provider %q", provider)
	}
}

// externalSearchProviderIDs lists the supported external search provider ids, for
// validation in the settings handler.
func externalSearchProviderIDs() []string {
	return []string{"tavily", "brave", "serper", "exa", "linkup", "searchapi", "youcom", "google-pse"}
}

func searchHTTP(ctx context.Context, method, url string, headers map[string]string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := GetRestClientForProxy("").Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("search HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func capResults(in []WebSearchResult) []WebSearchResult {
	if len(in) > maxWebSearchResults {
		return in[:maxWebSearchResults]
	}
	return in
}

// --- Tavily: POST /search, Bearer auth ---
func searchTavily(ctx context.Context, apiKey, query string) ([]WebSearchResult, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"query": query, "max_results": maxWebSearchResults, "search_depth": "basic",
	})
	raw, err := searchHTTP(ctx, "POST", "https://api.tavily.com/search",
		map[string]string{"Authorization": "Bearer " + apiKey}, body)
	if err != nil {
		return nil, err
	}
	var d struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := make([]WebSearchResult, 0, len(d.Results))
	for _, r := range d.Results {
		out = append(out, WebSearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return capResults(out), nil
}

// --- Brave: GET /res/v1/web/search, x-subscription-token ---
func searchBrave(ctx context.Context, apiKey, query string) ([]WebSearchResult, error) {
	u := "https://api.search.brave.com/res/v1/web/search?q=" + url.QueryEscape(query)
	raw, err := searchHTTP(ctx, "GET", u, map[string]string{"X-Subscription-Token": apiKey}, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := make([]WebSearchResult, 0, len(d.Web.Results))
	for _, r := range d.Web.Results {
		out = append(out, WebSearchResult{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return capResults(out), nil
}

// --- Serper: POST /search, x-api-key ---
func searchSerper(ctx context.Context, apiKey, query string) ([]WebSearchResult, error) {
	body, _ := json.Marshal(map[string]interface{}{"q": query, "num": maxWebSearchResults})
	raw, err := searchHTTP(ctx, "POST", "https://google.serper.dev/search",
		map[string]string{"X-API-KEY": apiKey}, body)
	if err != nil {
		return nil, err
	}
	var d struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := make([]WebSearchResult, 0, len(d.Organic))
	for _, r := range d.Organic {
		out = append(out, WebSearchResult{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return capResults(out), nil
}

// --- Exa: POST /search, x-api-key ---
func searchExa(ctx context.Context, apiKey, query string) ([]WebSearchResult, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"query": query, "numResults": maxWebSearchResults,
		"contents": map[string]interface{}{"text": map[string]interface{}{"maxCharacters": 500}},
	})
	raw, err := searchHTTP(ctx, "POST", "https://api.exa.ai/search",
		map[string]string{"x-api-key": apiKey}, body)
	if err != nil {
		return nil, err
	}
	var d struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := make([]WebSearchResult, 0, len(d.Results))
	for _, r := range d.Results {
		out = append(out, WebSearchResult{Title: r.Title, URL: r.URL, Snippet: r.Text})
	}
	return capResults(out), nil
}

// --- Linkup: POST /v1/search, Bearer ---
func searchLinkup(ctx context.Context, apiKey, query string) ([]WebSearchResult, error) {
	body, _ := json.Marshal(map[string]interface{}{"q": query, "depth": "standard", "outputType": "searchResults"})
	raw, err := searchHTTP(ctx, "POST", "https://api.linkup.so/v1/search",
		map[string]string{"Authorization": "Bearer " + apiKey}, body)
	if err != nil {
		return nil, err
	}
	var d struct {
		Results []struct {
			Name    string `json:"name"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := make([]WebSearchResult, 0, len(d.Results))
	for _, r := range d.Results {
		out = append(out, WebSearchResult{Title: r.Name, URL: r.URL, Snippet: r.Content})
	}
	return capResults(out), nil
}

// --- SearchAPI: GET /api/v1/search?engine=google, api_key query param ---
func searchSearchAPI(ctx context.Context, apiKey, query string) ([]WebSearchResult, error) {
	u := "https://www.searchapi.io/api/v1/search?engine=google&q=" + url.QueryEscape(query) + "&api_key=" + url.QueryEscape(apiKey)
	raw, err := searchHTTP(ctx, "GET", u, nil, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		OrganicResults []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic_results"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := make([]WebSearchResult, 0, len(d.OrganicResults))
	for _, r := range d.OrganicResults {
		out = append(out, WebSearchResult{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return capResults(out), nil
}

// --- You.com: GET /search, x-api-key ---
func searchYouCom(ctx context.Context, apiKey, query string) ([]WebSearchResult, error) {
	u := "https://api.ydc-index.io/search?query=" + url.QueryEscape(query)
	raw, err := searchHTTP(ctx, "GET", u, map[string]string{"X-API-Key": apiKey}, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Hits []struct {
			Title       string   `json:"title"`
			URL         string   `json:"url"`
			Description string   `json:"description"`
			Snippets    []string `json:"snippets"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := make([]WebSearchResult, 0, len(d.Hits))
	for _, r := range d.Hits {
		snippet := r.Description
		if snippet == "" && len(r.Snippets) > 0 {
			snippet = strings.Join(r.Snippets, " ")
		}
		out = append(out, WebSearchResult{Title: r.Title, URL: r.URL, Snippet: snippet})
	}
	return capResults(out), nil
}

// --- Google Programmable Search: GET /customsearch/v1, key=cx in apiKey "key:cx" ---
func searchGooglePSE(ctx context.Context, apiKey, query string) ([]WebSearchResult, error) {
	key, cx, found := strings.Cut(apiKey, ":")
	if !found {
		return nil, fmt.Errorf("google-pse api key must be in the form key:cx")
	}
	u := "https://www.googleapis.com/customsearch/v1?key=" + url.QueryEscape(key) +
		"&cx=" + url.QueryEscape(cx) + "&q=" + url.QueryEscape(query)
	raw, err := searchHTTP(ctx, "GET", u, nil, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := make([]WebSearchResult, 0, len(d.Items))
	for _, r := range d.Items {
		out = append(out, WebSearchResult{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return capResults(out), nil
}
