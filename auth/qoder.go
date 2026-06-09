package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
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
	"time"

	"github.com/google/uuid"
)

// Qoder device-token flow + COSY signing + WAF-bypass body encoding, ported
// from 9router (src/lib/qoder/{cosy,encoding,constants}.js,
// src/lib/oauth/services/qoder.js). Qoder gates inference behind a hybrid
// RSA+AES+MD5 signature plus a custom body obfuscation; both must match the
// upstream's validation byte-for-byte or it 401s, so this is a faithful port.
const (
	qoderOpenAPIBase   = "https://openapi.qoder.sh"
	qoderLoginURL      = "https://qoder.com/device/selectAccounts"
	qoderDeviceTokeURL = qoderOpenAPIBase + "/api/v1/deviceToken/poll"
	qoderUserInfoURL   = qoderOpenAPIBase + "/api/v1/userinfo"

	// COSY header constants — matched against the upstream signature validation.
	qoderIDEVersion   = "1.0.0"
	qoderClientType   = "5"
	qoderDataPolicy   = "disagree"
	qoderLoginVersion = "v2"
	qoderMachineOS    = "x86_64_windows"
	qoderMachineType  = "5"

	qoderLoginTimeout = 5 * time.Minute
)

// qoderRSAPublicKey is the COSY encryption key extracted from the Qoder IDE.
const qoderRSAPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var qoderRSAKey *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(qoderRSAPublicKey))
	if block == nil {
		return
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return
	}
	if k, ok := pub.(*rsa.PublicKey); ok {
		qoderRSAKey = k
	}
}

// ---- WAF-bypass body encoding ---------------------------------------------

const (
	qoderStdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
)

var qoderS2C = func() [128]byte {
	var t [128]byte
	for i := range t {
		t[i] = 0 // 0 means "no substitution" (we check membership separately)
	}
	var has [128]bool
	for i := 0; i < 64; i++ {
		t[qoderStdAlphabet[i]] = qoderCustomAlphabet[i]
		has[qoderStdAlphabet[i]] = true
	}
	t['='] = '$'
	has['='] = true
	qoderS2CHas = has
	return t
}()

var qoderS2CHas [128]bool

// QoderEncodeBody applies Qoder's WAF-bypass scheme: standard base64, then
// rearrange into [tail][mid][head] thirds, then substitute via the custom
// alphabet. The result must be sent with &Encode=1 in the URL. Returns the
// encoded bytes (latin1 representation of the substituted string).
func QoderEncodeBody(plaintext []byte) []byte {
	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3
	// [tail][mid][head]: std[n-a:] + std[a:n-a] + std[:a]
	rearranged := std[n-a:] + std[a:n-a] + std[:a]

	out := make([]byte, n)
	for i := 0; i < n; i++ {
		c := rearranged[i]
		if c < 128 && qoderS2CHas[c] {
			out[i] = qoderS2C[c]
		} else {
			out[i] = c
		}
	}
	return out
}

// ---- COSY signing ----------------------------------------------------------

// QoderCreds is the stable identity COSY signing needs per request.
type QoderCreds struct {
	UserID    string
	AuthToken string // device access token (dt-...)
	Name      string
	Email     string
	MachineID string
}

// BuildCosyHeaders builds the full Cosy-* header set for a single signed Qoder
// request over the EXACT body bytes that will be sent (the encoded body).
// requestURL is used to derive the sigPath (the path with the leading "/algo"
// stripped). Returns the header map, or an error if creds are incomplete / the
// RSA key failed to load.
func BuildCosyHeaders(body []byte, requestURL string, creds QoderCreds) (map[string]string, error) {
	if strings.TrimSpace(creds.UserID) == "" {
		return nil, fmt.Errorf("cosy: user id is empty")
	}
	if strings.TrimSpace(creds.AuthToken) == "" {
		return nil, fmt.Errorf("cosy: auth token is empty")
	}
	if qoderRSAKey == nil {
		return nil, fmt.Errorf("cosy: RSA public key not loaded")
	}

	cosyKey, info, err := qoderEncryptUserInfo(creds)
	if err != nil {
		return nil, err
	}

	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	requestID := uuid.New().String()

	payloadJSON, _ := json.Marshal(map[string]interface{}{
		"version":     "v1",
		"requestId":   requestID,
		"info":        info,
		"cosyVersion": qoderIDEVersion,
		"ideVersion":  "",
	})
	payloadB64 := base64.StdEncoding.EncodeToString(payloadJSON)

	sigPath := qoderComputeSigPath(requestURL)
	// sig = md5hex( payloadB64 \n cosyKey \n timestamp \n body \n sigPath ), with
	// body treated as latin1 bytes (already raw bytes here).
	var sb strings.Builder
	sb.WriteString(payloadB64)
	sb.WriteString("\n")
	sb.WriteString(cosyKey)
	sb.WriteString("\n")
	sb.WriteString(timestamp)
	sb.WriteString("\n")
	sb.Write(body)
	sb.WriteString("\n")
	sb.WriteString(sigPath)
	sig := fmt.Sprintf("%x", md5.Sum([]byte(sb.String())))

	machineID := creds.MachineID
	if machineID == "" {
		machineID = uuid.New().String()
	}
	bodyHash := fmt.Sprintf("%x", md5.Sum(body))
	bodyLength := fmt.Sprintf("%d", len(body))

	return map[string]string{
		"Authorization":          "Bearer COSY." + payloadB64 + "." + sig,
		"Cosy-Key":               cosyKey,
		"Cosy-User":              creds.UserID,
		"Cosy-Date":              timestamp,
		"Cosy-Version":           qoderIDEVersion,
		"Cosy-Machineid":         machineID,
		"Cosy-Machinetoken":      machineID,
		"Cosy-Machinetype":       qoderMachineType,
		"Cosy-Machineos":         qoderMachineOS,
		"Cosy-Clienttype":        qoderClientType,
		"Cosy-Clientip":          "127.0.0.1",
		"Cosy-Bodyhash":          bodyHash,
		"Cosy-Bodylength":        bodyLength,
		"Cosy-Sigpath":           sigPath,
		"Cosy-Data-Policy":       qoderDataPolicy,
		"Cosy-Organization-Id":   "",
		"Cosy-Organization-Tags": "",
		"Login-Version":          qoderLoginVersion,
		"X-Request-Id":           uuid.New().String(),
	}, nil
}

// qoderEncryptUserInfo AES-128-CBC encrypts the user-info JSON with a fresh
// 16-char key (IV = the key bytes, matching qodercli), and RSA-encrypts the AES
// key. Returns (rsaWrappedKeyB64, aesEncryptedInfoB64).
func qoderEncryptUserInfo(creds QoderCreds) (cosyKey, info string, err error) {
	aesKey := uuid.New().String()[:16] // 16 chars of a UUID string (incl. hyphens)
	plaintext, _ := json.Marshal(map[string]string{
		"uid":                  creds.UserID,
		"security_oauth_token": creds.AuthToken,
		"name":                 creds.Name,
		"aid":                  "",
		"email":                creds.Email,
	})
	infoB64, err := qoderAESEncryptCBC(plaintext, []byte(aesKey))
	if err != nil {
		return "", "", err
	}
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, qoderRSAKey, []byte(aesKey))
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(wrapped), infoB64, nil
}

// qoderAESEncryptCBC AES-128-CBC encrypts with IV = the key bytes (matching
// qodercli/Veria), PKCS7-padded, base64-encoded.
func qoderAESEncryptCBC(plaintext, key []byte) (string, error) {
	if len(key) != 16 {
		return "", fmt.Errorf("aes key must be 16 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	iv := key[:aes.BlockSize]
	padded := qoderPKCS7Pad(plaintext, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

func qoderPKCS7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - (len(data) % blockSize)
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

// qoderComputeSigPath strips the leading "/algo" from the request URL's path.
func qoderComputeSigPath(requestURL string) string {
	u, err := url.Parse(requestURL)
	if err != nil {
		return ""
	}
	p := u.Path
	if strings.HasPrefix(p, "/algo") {
		return p[len("/algo"):]
	}
	return p
}

// ---- Device-token flow -----------------------------------------------------

// QoderSession is an in-flight Qoder device login.
type QoderSession struct {
	ID           string
	AuthURL      string
	CodeVerifier string
	Nonce        string
	MachineID    string
	ExpiresAt    time.Time
}

// StartQoderLogin initiates the device flow: generates PKCE + nonce + machineId,
// builds the browser URL, and returns the session for polling. (No local server:
// Qoder's flow is poll-based, not redirect-based.)
func StartQoderLogin() *QoderSession {
	verifier := qoderBase64URL(randomBytes(32))
	h := sha256.Sum256([]byte(verifier))
	challenge := qoderBase64URL(h[:])
	nonce := uuid.New().String()
	machineID := uuid.New().String()

	params := url.Values{}
	params.Set("challenge", challenge)
	params.Set("challenge_method", "S256")
	params.Set("machine_id", machineID)
	params.Set("nonce", nonce)

	return &QoderSession{
		ID:           GenerateAccountID(),
		AuthURL:      qoderLoginURL + "?" + params.Encode(),
		CodeVerifier: verifier,
		Nonce:        nonce,
		MachineID:    machineID,
		ExpiresAt:    time.Now().Add(qoderLoginTimeout),
	}
}

// QoderDeviceToken is the result of a successful device-token poll.
type QoderDeviceToken struct {
	AccessToken  string
	RefreshToken string
	UserID       string
	ExpiresAt    int64 // Unix seconds
}

// PollQoderDeviceToken performs one poll. status is "pending" (keep polling),
// "ok" (token captured), or "" with err on terminal failure.
func PollQoderDeviceToken(nonce, codeVerifier string) (status string, token *QoderDeviceToken, err error) {
	if nonce == "" || codeVerifier == "" {
		return "", nil, fmt.Errorf("missing nonce or code verifier")
	}
	u := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		qoderDeviceTokeURL, url.QueryEscape(nonce), url.QueryEscape(codeVerifier))

	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "pending", nil, nil // transient — caller retries
	}
	defer resp.Body.Close()

	// 202/404 mean "keep polling".
	if resp.StatusCode == 202 || resp.StatusCode == 404 {
		return "pending", nil, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("qoder device token poll failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	var b struct {
		Token        string      `json:"token"`
		RefreshToken string      `json:"refresh_token"`
		UserID       string      `json:"user_id"`
		ExpiresAt    interface{} `json:"expires_at"`
		ExpiresIn    int64       `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return "", nil, fmt.Errorf("qoder device token poll: invalid JSON: %w", err)
	}
	if b.Token == "" {
		return "", nil, fmt.Errorf("qoder device token poll returned 200 but no token")
	}
	return "ok", &QoderDeviceToken{
		AccessToken:  b.Token,
		RefreshToken: b.RefreshToken,
		UserID:       b.UserID,
		ExpiresAt:    qoderParseExpiry(b.ExpiresAt, b.ExpiresIn),
	}, nil
}

// FetchQoderUserInfo fetches profile info (best-effort) for display.
func FetchQoderUserInfo(accessToken string) (name, email, orgID string) {
	req, _ := http.NewRequest("GET", qoderUserInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var b struct {
		Name           string `json:"name"`
		Username       string `json:"username"`
		Email          string `json:"email"`
		OrganizationID string `json:"organization_id"`
	}
	json.Unmarshal(body, &b)
	name = strings.TrimSpace(firstNonEmptyStr(b.Name, b.Username))
	return name, strings.TrimSpace(b.Email), strings.TrimSpace(b.OrganizationID)
}

// qoderParseExpiry converts the upstream expiry hint into a Unix-SECONDS
// timestamp (we store ExpiresAt in seconds, unlike 9router's ms). Accepts a
// numeric ms-epoch, a numeric string, an RFC3339 string, or expires_in seconds;
// defaults to now + 30 days.
func qoderParseExpiry(expiresAt interface{}, expiresInSeconds int64) int64 {
	switch v := expiresAt.(type) {
	case float64:
		if v > 0 {
			return int64(v) / 1000 // ms -> s
		}
	case string:
		s := strings.TrimSpace(v)
		if s != "" {
			if isAllDigits(s) {
				// ms-epoch string
				var ms int64
				fmt.Sscanf(s, "%d", &ms)
				if ms > 0 {
					return ms / 1000
				}
			}
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t.Unix()
			}
		}
	}
	if expiresInSeconds >= 0 && expiresInSeconds != 0 {
		return time.Now().Unix() + expiresInSeconds
	}
	return time.Now().Add(30 * 24 * time.Hour).Unix()
}

func qoderBase64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
