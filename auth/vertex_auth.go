package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Vertex AI Service-Account auth, ported in spirit from 9router's VertexExecutor.
// A Google Cloud Service Account JSON key authenticates by self-signing a JWT
// assertion with the SA's RSA private key and exchanging it at the Google token
// endpoint (urn:ietf:params:oauth:grant-type:jwt-bearer) for a short-lived OAuth2
// access token. That token is the Bearer credential for Vertex AI's regional
// aiplatform.googleapis.com endpoints. Tokens are cached per-SA until ~1 min before
// expiry. No browser flow — the operator pastes the SA JSON once.
const (
	vertexTokenURL = "https://oauth2.googleapis.com/token"
	vertexScope    = "https://www.googleapis.com/auth/cloud-platform"
	vertexTokenTTL = 3600 // Google SA access tokens last 1h
)

// VertexServiceAccount is the subset of a GCP SA JSON key we need.
type VertexServiceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// ParseVertexServiceAccount validates and parses an SA JSON key.
func ParseVertexServiceAccount(saJSON string) (*VertexServiceAccount, error) {
	var sa VertexServiceAccount
	if err := json.Unmarshal([]byte(saJSON), &sa); err != nil {
		return nil, fmt.Errorf("invalid service account JSON: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" || sa.ProjectID == "" {
		return nil, fmt.Errorf("service account JSON missing client_email/private_key/project_id")
	}
	if _, err := parseRSAPrivateKey(sa.PrivateKey); err != nil {
		return nil, fmt.Errorf("service account private_key not parseable: %w", err)
	}
	return &sa, nil
}

type vertexCachedToken struct {
	token     string
	expiresAt time.Time
}

var (
	vertexTokenCache   = map[string]vertexCachedToken{} // client_email -> token
	vertexTokenCacheMu sync.Mutex
)

// VertexAccessToken returns a valid OAuth2 access token for the SA, minting (and
// caching) a fresh one via the JWT-bearer grant when needed.
func VertexAccessToken(ctx context.Context, saJSON string) (string, time.Time, error) {
	sa, err := ParseVertexServiceAccount(saJSON)
	if err != nil {
		return "", time.Time{}, err
	}
	vertexTokenCacheMu.Lock()
	if c, ok := vertexTokenCache[sa.ClientEmail]; ok && time.Until(c.expiresAt) > time.Minute {
		vertexTokenCacheMu.Unlock()
		return c.token, c.expiresAt, nil
	}
	vertexTokenCacheMu.Unlock()

	assertion, err := buildVertexJWT(sa)
	if err != nil {
		return "", time.Time{}, err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	tokenURL := sa.TokenURI
	if tokenURL == "" {
		tokenURL = vertexTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("vertex token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", time.Time{}, fmt.Errorf("vertex token request failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	var d struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return "", time.Time{}, fmt.Errorf("vertex token parse failed: %w", err)
	}
	if d.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("vertex token response missing access_token")
	}
	ttl := d.ExpiresIn
	if ttl <= 0 {
		ttl = vertexTokenTTL
	}
	expiresAt := time.Now().Add(time.Duration(ttl) * time.Second)
	vertexTokenCacheMu.Lock()
	vertexTokenCache[sa.ClientEmail] = vertexCachedToken{token: d.AccessToken, expiresAt: expiresAt}
	vertexTokenCacheMu.Unlock()
	return d.AccessToken, expiresAt, nil
}

// buildVertexJWT builds and RS256-signs the SA assertion JWT.
func buildVertexJWT(sa *VertexServiceAccount) (string, error) {
	now := time.Now()
	header := map[string]interface{}{"alg": "RS256", "typ": "JWT"}
	claims := map[string]interface{}{
		"iss":   sa.ClientEmail,
		"scope": vertexScope,
		"aud":   firstNonEmptyStr(sa.TokenURI, vertexTokenURL),
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)

	key, err := parseRSAPrivateKey(sa.PrivateKey)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", fmt.Errorf("vertex JWT signing failed: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA private key")
	}
	return rsaKey, nil
}
