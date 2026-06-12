package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Cursor checksum + header builder, ported from 9router's
// open-sse/utils/cursorChecksum.js. Cursor's API requires an x-cursor-checksum
// header derived from a timestamp obfuscated with the "Jyh cipher" (a rolling-key
// XOR), suffixed with the machine id, plus a set of x-cursor-* identity headers.

const cursorURLSafeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// cursorNowISO returns the current time in RFC3339 (ISO-8601), matching JS
// new Date().toISOString().
func cursorNowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// cursorHashed64Hex returns sha256hex(input + salt), matching generateHashed64Hex.
func cursorHashed64Hex(input, salt string) string {
	h := sha256.Sum256([]byte(input + salt))
	return hex.EncodeToString(h[:])
}

// cursorSessionID returns a UUIDv5 (DNS namespace) of the auth token, matching
// generateSessionId.
func cursorSessionID(authToken string) string {
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(authToken)).String()
}

// cursorChecksum implements the Jyh cipher over a 6-byte big-endian timestamp
// (Math.floor(Date.now()/1e6)), URL-safe-base64 encodes it, and suffixes the
// machine id. Byte-compatible with generateCursorChecksum.
func cursorChecksum(machineID string) string {
	ts := time.Now().UnixMilli() / 1000000 // floor(ms / 1e6)
	b := []byte{
		byte((ts >> 40) & 0xff),
		byte((ts >> 32) & 0xff),
		byte((ts >> 24) & 0xff),
		byte((ts >> 16) & 0xff),
		byte((ts >> 8) & 0xff),
		byte(ts & 0xff),
	}
	t := byte(165)
	for i := 0; i < len(b); i++ {
		b[i] = byte((int(b[i]^t) + (i % 256)) & 0xff)
		t = b[i]
	}
	// URL-safe base64 without padding, manual (matches the JS loop exactly).
	var enc []byte
	for i := 0; i < len(b); i += 3 {
		a := b[i]
		var bb, cc byte
		if i+1 < len(b) {
			bb = b[i+1]
		}
		if i+2 < len(b) {
			cc = b[i+2]
		}
		enc = append(enc, cursorURLSafeAlphabet[a>>2])
		enc = append(enc, cursorURLSafeAlphabet[((a&3)<<4)|(bb>>4)])
		if i+1 < len(b) {
			enc = append(enc, cursorURLSafeAlphabet[((bb&15)<<2)|(cc>>6)])
		}
		if i+2 < len(b) {
			enc = append(enc, cursorURLSafeAlphabet[cc&63])
		}
	}
	return string(enc) + machineID
}

// buildCursorHeaders constructs the full Cursor request header set. machineID may be
// empty, in which case it's derived from the token (matching the JS fallback).
func buildCursorHeaders(accessToken, machineID string, ghostMode bool) map[string]string {
	clean := accessToken
	if i := strings.Index(accessToken, "::"); i >= 0 {
		clean = accessToken[i+2:]
	}
	effMachine := machineID
	if effMachine == "" {
		effMachine = cursorHashed64Hex(clean, "machineId")
	}
	ghost := "true"
	if !ghostMode {
		ghost = "false"
	}
	return map[string]string{
		"authorization":            "Bearer " + clean,
		"connect-accept-encoding":  "gzip",
		"connect-protocol-version": "1",
		"content-type":             "application/connect+proto",
		"user-agent":               "connect-es/1.6.1",
		"x-amzn-trace-id":          "Root=" + uuid.New().String(),
		"x-client-key":             cursorHashed64Hex(clean, ""),
		"x-cursor-checksum":        cursorChecksum(effMachine),
		"x-cursor-client-version":  "3.1.0",
		"x-cursor-client-type":     "ide",
		"x-cursor-client-os":       "linux",
		"x-cursor-client-arch":     "x64",
		"x-cursor-client-device-type": "desktop",
		"x-cursor-config-version":  uuid.New().String(),
		"x-cursor-timezone":        "UTC",
		"x-ghost-mode":             ghost,
		"x-request-id":             uuid.New().String(),
		"x-session-id":             cursorSessionID(clean),
	}
}
