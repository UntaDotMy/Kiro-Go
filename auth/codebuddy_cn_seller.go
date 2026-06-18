// Package auth provides the seller client for CodeBuddy CN reseller credential
// fetching. It ports the ServerFetchDialog / _fetch_one / _import_selected flow
// from codebuddychina/_internal/src/ui/pages/accounts.py into Go, using the
// existing httpClient() + GenerateAccountID helpers already in this package.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

// CodeBuddyCNSellerURL is the main credential server (card-key/phone-url/sub-key
// resolution). Exported so config can override later.
var CodeBuddyCNSellerURL = "http://103.36.63.44:9658"

// CodeBuddyCNCKServerURL is the cookie/CK server (optional web billing fallback).
var CodeBuddyCNCKServerURL = "http://124.222.75.216:9658"

// CodeBuddyCNCKAPIKey is the static CK server API key used by the reseller tools.
var CodeBuddyCNCKAPIKey = "ck_client_2026ok"

// parsedItem classifies one input line for seller dispatch.
type parsedItem struct {
	kind  string // "sub_api_key", "phone_url", "card_code"
	value string
	raw   string
}

// CodeBuddyCNFetched is one resolved account from the seller servers.
type CodeBuddyCNFetched struct {
	Phone     string // phone number (account uid)
	APIKey    string // ck_... API key usable as Bearer token for inference
	LoginURL  string // copilot.tencent.com login URL (canonical auth URL)
	WebCookie string // optional browser cookie JSON (may be empty)
}

// FetchCodeBuddyCNAccounts takes a multiline string of card-keys/phone-urls/sub-keys
// and resolves them into ck_ API key accounts through the seller servers. Each line
// is classified as sub_api_key (starts with "sk_"), phone_url (contains "----"), or
// card_code (anything else). Partial success is acceptable — entries without a
// resulting ck_ key are logged and skipped. The caller must close the context on
// cancellation. All seller responses are bounded to 4 MiB.
func FetchCodeBuddyCNAccounts(ctx context.Context, lines []string) ([]CodeBuddyCNFetched, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	items := make([]parsedItem, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "sk_") {
			items = append(items, parsedItem{kind: "sub_api_key", value: line, raw: line})
		} else if strings.Contains(line, "----") {
			items = append(items, parsedItem{kind: "phone_url", value: line, raw: line})
		} else {
			items = append(items, parsedItem{kind: "card_code", value: line, raw: line})
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no valid items in input")
	}

	var out []CodeBuddyCNFetched
	for _, item := range items {
		fetched, err := fetchOneCNAccount(ctx, item)
		if err != nil {
			logger.Warnf("[CodeBuddyCN] seller fetch failed for %q: %v", item.raw, err)
			continue
		}
		if fetched.APIKey == "" {
			logger.Warnf("[CodeBuddyCN] seller returned no api_key for %q", item.raw)
			continue
		}
		out = append(out, fetched)
	}
	return out, nil
}

func fetchOneCNAccount(ctx context.Context, item parsedItem) (CodeBuddyCNFetched, error) {
	result := CodeBuddyCNFetched{}

	queryItems := []parsedItem{item}

	if item.kind == "sub_api_key" {
		expanded, err := sellerGetActiveKeys(ctx, item.value)
		if err != nil {
			return result, err
		}
		if len(expanded) == 0 {
			return result, fmt.Errorf("sub key has no active keys")
		}
		queryItems = expanded
	}

	accounts, err := sellerWebBatchQuery(ctx, queryItems)
	if err != nil {
		return result, err
	}

	apiKeys, _ := sellerWebBatchGetAPIKeys(ctx, queryItems)

	loginURLMap := make(map[string]string)
	for _, qi := range queryItems {
		if qi.kind == "phone_url" && strings.Contains(qi.value, "----") {
			parts := strings.SplitN(qi.value, "----", 2)
			if len(parts) == 2 {
				loginURLMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	type accTemp struct {
		phone    string
		apiKey   string
		loginURL string
	}
	var flat []accTemp
	for _, a := range accounts {
		phone := a.phone
		key := a.apiKey
		if key == "" {
			if k, ok := apiKeys[phone]; ok {
				key = k
			}
		}
		url := ""
		if u, ok := loginURLMap[phone]; ok {
			url = u
		}
		flat = append(flat, accTemp{phone, key, url})
	}

	if len(flat) == 0 {
		return result, fmt.Errorf("no accounts resolved")
	}

	phones := make([]string, 0, len(flat))
	for _, f := range flat {
		phones = append(phones, f.phone)
	}
	cookies, _ := sellerBatchGetCookies(ctx, phones)

	cookieMap := make(map[string]string)
	for _, c := range cookies {
		cookieMap[c.phone] = c.cookieData
	}

	primary := flat[0]
	result.Phone = primary.phone
	result.APIKey = primary.apiKey
	result.LoginURL = primary.loginURL
	if ck, ok := cookieMap[primary.phone]; ok {
		result.WebCookie = ck
	}
	return result, nil
}

// ---- Seller API calls ----

func sellerPost(ctx context.Context, url string, body interface{}, timeout time.Duration) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func sellerGetActiveKeys(ctx context.Context, subAPIKey string) ([]parsedItem, error) {
	body, err := sellerPost(ctx, CodeBuddyCNSellerURL+"/api/get_active_keys",
		map[string]string{"sub_api_key": subAPIKey}, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("get_active_keys: %w", err)
	}
	var resp struct {
		Success    bool `json:"success"`
		ActiveKeys []struct {
			Phone  string `json:"phone"`
			APIURL string `json:"api_url"`
		} `json:"active_keys"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("get_active_keys parse: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("get_active_keys: seller rejected sub key")
	}
	var out []parsedItem
	for _, ak := range resp.ActiveKeys {
		purl := ak.Phone
		if ak.APIURL != "" {
			purl = ak.Phone + "----" + ak.APIURL
		}
		out = append(out, parsedItem{kind: "phone_url", value: purl, raw: purl})
	}
	return out, nil
}

func sellerWebBatchQuery(ctx context.Context, items []parsedItem) ([]struct{ phone, apiKey string }, error) {
	mapped := make([]map[string]string, 0, len(items))
	for _, it := range items {
		m := make(map[string]string)
		if it.kind == "card_code" {
			m["card_code"] = it.value
		} else {
			m["phone_url"] = it.value
		}
		mapped = append(mapped, m)
	}
	body, err := sellerPost(ctx, CodeBuddyCNSellerURL+"/api/web_batch_query",
		map[string]interface{}{"items": mapped}, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("web_batch_query: %w", err)
	}
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Results []struct {
			Success bool   `json:"success"`
			Phone   string `json:"phone"`
			Key     string `json:"key"`
			APIKey  string `json:"api_key"`
			Message string `json:"message"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("web_batch_query parse: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("web_batch_query: %s", resp.Message)
	}
	var out []struct{ phone, apiKey string }
	for _, r := range resp.Results {
		if !r.Success {
			return nil, fmt.Errorf("web_batch_query item failed: %s", r.Message)
		}
		out = append(out, struct{ phone, apiKey string }{r.Phone, r.APIKey})
	}
	return out, nil
}

func sellerWebBatchGetAPIKeys(ctx context.Context, items []parsedItem) (map[string]string, error) {
	type cred struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	creds := make([]cred, 0, len(items))
	for _, it := range items {
		if it.kind == "card_code" {
			creds = append(creds, cred{Type: "card_code", Value: it.value})
		} else {
			creds = append(creds, cred{Type: "phone_url", Value: it.value})
		}
	}
	body, err := sellerPost(ctx, CodeBuddyCNSellerURL+"/api/web_batch_get_api_keys",
		map[string]interface{}{"keys": creds}, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("web_batch_get_api_keys: %w", err)
	}

	var resp struct {
		Success bool `json:"success"`
		Data    []struct {
			Phone  string `json:"phone"`
			APIKey string `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("web_batch_get_api_keys parse: %w", err)
	}
	out := make(map[string]string)
	if resp.Success {
		for _, d := range resp.Data {
			if d.Phone != "" && d.APIKey != "" {
				out[d.Phone] = d.APIKey
			}
		}
	}
	return out, nil
}

func sellerBatchGetCookies(ctx context.Context, phones []string) ([]struct{ phone, cookieData string }, error) {
	body, err := sellerPost(ctx, CodeBuddyCNSellerURL+"/api/batch_get_cookies",
		map[string]interface{}{"phones": phones}, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("batch_get_cookies: %w", err)
	}

	var resp struct {
		Success  bool `json:"success"`
		Accounts []struct {
			Phone      string `json:"phone"`
			CookieData string `json:"cookie_data"`
			APIURL     string `json:"api_url"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("batch_get_cookies parse: %w", err)
	}
	var out []struct{ phone, cookieData string }
	if resp.Success {
		for _, a := range resp.Accounts {
			if a.Phone != "" && a.CookieData != "" {
				out = append(out, struct{ phone, cookieData string }{a.Phone, a.CookieData})
			}
		}
	}
	return out, nil
}
