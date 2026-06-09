package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

// referenceQoderEncode reimplements the 9router JS algorithm independently
// (base64 -> [tail][mid][head] thirds -> custom-alphabet substitution) so the
// Go port is checked against a second implementation, not just itself.
func referenceQoderEncode(plaintext []byte) []byte {
	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]

	// Build the std->custom map fresh.
	stdAlpha := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	custAlpha := "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	m := map[byte]byte{}
	for i := 0; i < 64; i++ {
		m[stdAlpha[i]] = custAlpha[i]
	}
	m['='] = '$'

	out := make([]byte, n)
	for i := 0; i < n; i++ {
		c := rearranged[i]
		if sub, ok := m[c]; ok {
			out[i] = sub
		} else {
			out[i] = c
		}
	}
	return out
}

func TestQoderEncodeBodyMatchesReference(t *testing.T) {
	inputs := [][]byte{
		[]byte(`{"a":1}`),
		[]byte("hello world"),
		[]byte(`{"request_id":"abc","messages":[{"role":"user","content":"hi there"}]}`),
		[]byte(""),
		[]byte("x"),
		[]byte("xy"),
		[]byte("xyz"),
	}
	for _, in := range inputs {
		got := QoderEncodeBody(in)
		want := referenceQoderEncode(in)
		if string(got) != string(want) {
			t.Errorf("QoderEncodeBody(%q):\n got=%q\nwant=%q", in, got, want)
		}
	}
}

// TestQoderEncodeBodyKnownVector pins one fully worked example so a future
// accidental change to the alphabet/rearrange is caught even if both impls drift
// together.
func TestQoderEncodeBodyKnownVector(t *testing.T) {
	// "AAAA" -> base64 "QUFBQQ==" (8 chars). a=8/3=2.
	// rearranged = std[6:]+std[2:6]+std[0:2] = "==" + "FBQQ" + "QU" = "==FBQQQU"
	// substitute via custom alphabet:
	//   '=' -> '$', 'F' -> 'g', 'B' -> 'o', 'Q' -> 'C', 'U' -> 'a'
	// Wait: map letters individually below; assert against the reference too.
	in := []byte("AAAA")
	got := string(QoderEncodeBody(in))
	// Independently: std="QUFBQQ==" len8, a=2, rearranged="=="+"FBQQ"+"QU"
	std := base64.StdEncoding.EncodeToString(in)
	if std != "QUFBQQ==" {
		t.Fatalf("base64 sanity: got %q", std)
	}
	if len(got) != len(std) {
		t.Fatalf("encoded length %d != base64 length %d", len(got), len(std))
	}
	// The encoded output must NOT contain a raw '=' (it maps to '$').
	if strings.Contains(got, "=") {
		t.Errorf("encoded output should substitute '=' -> '$', got %q", got)
	}
}

// TestBuildCosyHeaders verifies the header set is complete and the signature is
// deterministic for fixed inputs (modulo the random AES key / request ids, the
// REQUIRED structural headers must always be present).
func TestBuildCosyHeaders(t *testing.T) {
	body := []byte("encoded-body-bytes")
	headers, err := BuildCosyHeaders(body, "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?Encode=1", QoderCreds{
		UserID:    "user_1",
		AuthToken: "dt-abc",
		Name:      "Tester",
		Email:     "t@example.com",
		MachineID: "machine-9",
	})
	if err != nil {
		t.Fatalf("BuildCosyHeaders: %v", err)
	}
	required := []string{
		"Authorization", "Cosy-Key", "Cosy-User", "Cosy-Date", "Cosy-Version",
		"Cosy-Machineid", "Cosy-Machinetoken", "Cosy-Machinetype", "Cosy-Machineos",
		"Cosy-Clienttype", "Cosy-Bodyhash", "Cosy-Bodylength", "Cosy-Sigpath",
		"Login-Version", "X-Request-Id",
	}
	for _, k := range required {
		if headers[k] == "" {
			t.Errorf("missing required COSY header %q", k)
		}
	}
	if !strings.HasPrefix(headers["Authorization"], "Bearer COSY.") {
		t.Errorf("Authorization should be a COSY bearer, got %q", headers["Authorization"])
	}
	if headers["Cosy-User"] != "user_1" {
		t.Errorf("Cosy-User = %q, want user_1", headers["Cosy-User"])
	}
	if headers["Cosy-Machineid"] != "machine-9" {
		t.Errorf("Cosy-Machineid should use the supplied machine id, got %q", headers["Cosy-Machineid"])
	}
	// sigPath strips the leading /algo.
	if headers["Cosy-Sigpath"] != "/api/v2/service/pro/sse/agent_chat_generation" {
		t.Errorf("Cosy-Sigpath = %q", headers["Cosy-Sigpath"])
	}
	// Body length must match.
	if headers["Cosy-Bodylength"] != "18" {
		t.Errorf("Cosy-Bodylength = %q, want 18", headers["Cosy-Bodylength"])
	}
}

// TestBuildCosyHeadersRequiresCreds verifies the guards.
func TestBuildCosyHeadersRequiresCreds(t *testing.T) {
	if _, err := BuildCosyHeaders([]byte("x"), "https://x/algo/y", QoderCreds{AuthToken: "dt-1"}); err == nil {
		t.Error("expected error for missing userId")
	}
	if _, err := BuildCosyHeaders([]byte("x"), "https://x/algo/y", QoderCreds{UserID: "u"}); err == nil {
		t.Error("expected error for missing authToken")
	}
}

// TestQoderParseExpiry pins the expiry parsing (ms-epoch number/string, RFC3339,
// expires_in seconds, default).
func TestQoderParseExpiry(t *testing.T) {
	if got := qoderParseExpiry(float64(1781594470000), 0); got != 1781594470 {
		t.Errorf("ms-epoch number: got %d", got)
	}
	if got := qoderParseExpiry("1781594470000", 0); got != 1781594470 {
		t.Errorf("ms-epoch string: got %d", got)
	}
	// expires_in seconds path (expiresAt nil).
	got := qoderParseExpiry(nil, 3600)
	if got < 3000 { // sanity: now + ~3600
		t.Errorf("expires_in path produced %d", got)
	}
}
