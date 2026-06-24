package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// debugCaptureDir returns the directory debug captures are written to, or "" when
// capture is disabled. Capture is on when the admin Debug Mode toggle is set
// (config.GetDebugCapture) OR the CODEBUDDY_CN_DUMP env var names a directory —
// the env var is kept as a no-restart escape hatch.
func debugCaptureDir() string {
	if env := strings.TrimSpace(os.Getenv("CODEBUDDY_CN_DUMP")); env != "" {
		return env
	}
	if config.GetDebugCapture() {
		return config.GetDebugCaptureDir()
	}
	return ""
}

// dumpOutboundBody writes a final outbound request body to the debug-capture
// directory, one timestamped file per call tagged with the backend id and a "req"
// marker. It exists to debug the cross-client tool-use regression and the
// CodeBuddy "sensitive content" rejection: dumping the same client's request for
// several backends (codebuddy-cn, codebuddy-ai, kiro) shows exactly what the
// neutralization filter kept or removed per provider. No-op unless debug capture
// is enabled; never fails the request.
func dumpOutboundBody(backend string, body []byte) {
	writeDebugCapture(backend, "req", body)
}

// dumpUpstreamResponse writes a raw upstream response body to the debug-capture
// directory tagged with a "resp" marker, so the exact bytes the provider returned
// (tool_call shape, moderation refusal) can be inspected alongside the request.
func dumpUpstreamResponse(backend string, body []byte) {
	writeDebugCapture(backend, "resp", body)
}

func writeDebugCapture(backend, kind string, body []byte) {
	dir := debugCaptureDir()
	if dir == "" {
		return
	}
	safe := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(backend)
	tag := fmt.Sprintf("%s-%s-%s-%s", safe, kind, time.Now().Format("20060102-150405.000"), dumpRandHex(4))

	// Also print the full body inline to the log so it can be copy-pasted straight
	// from `docker compose logs` without reading the file off disk. Framed with
	// BEGIN/END markers (grep "[debug]") so the exact bytes are unambiguous.
	logDebugBody(tag, body)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Warnf("[debug] dir create failed: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, tag+".json"), body, 0o600); err != nil {
		logger.Warnf("[debug] write failed: %v", err)
		return
	}
	logger.Infof("[debug] wrote %s.json (%d bytes)", tag, len(body))
}

// logDebugBody prints a captured body to the log between BEGIN/END markers so the
// operator can copy it directly out of docker logs. A very large body is split
// into numbered chunks because some log drivers truncate a single huge line.
func logDebugBody(tag string, body []byte) {
	const chunk = 16 * 1024
	logger.Infof("[debug] ===== BEGIN %s (%d bytes) =====", tag, len(body))
	if len(body) <= chunk {
		logger.Infof("[debug] %s | %s", tag, string(body))
	} else {
		parts := (len(body) + chunk - 1) / chunk
		for i := 0; i < parts; i++ {
			start := i * chunk
			end := start + chunk
			if end > len(body) {
				end = len(body)
			}
			logger.Infof("[debug] %s [%d/%d] | %s", tag, i+1, parts, string(body[start:end]))
		}
	}
	logger.Infof("[debug] ===== END %s =====", tag)
}

func dumpRandHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "rand"
	}
	return hex.EncodeToString(b)
}

// boundedBuffer is a goroutine-unsafe byte buffer that caps total capacity:
// writes past the cap are dropped (not errored), keeping the most recent bytes
// when the cap is exceeded. It implements io.Writer for use as an io.TeeReader
// sink so a raw upstream SSE stream can be captured for diagnosis without
// risking unbounded memory on a long generation. Bytes() returns a copy safe
// to hand to dumpUpstreamResponse after the stream ends.
type boundedBuffer struct {
	cap   int
	buf   []byte
	dropped int
}

// newBoundedBuffer returns a buffer that holds at most capBytes bytes; once full
// it evicts the oldest data to make room for new writes (ring-style) so the
// TAIL of a long stream is preserved (the tail is where truncation/completion
// behavior is most visible).
func newBoundedBuffer(capBytes int) *boundedBuffer {
	if capBytes <= 0 {
		capBytes = 1 << 20
	}
	return &boundedBuffer{cap: capBytes, buf: make([]byte, 0, capBytes)}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.buf)+n <= b.cap {
		b.buf = append(b.buf, p...)
		return n, nil
	}
	// Over cap: keep the most recent b.cap bytes. Append what fits after evicting
	// the oldest excess.
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.cap {
		evict := len(b.buf) - b.cap
		b.dropped += evict
		// Copy the tail to the front and reslice.
		copy(b.buf, b.buf[evict:])
		b.buf = b.buf[:b.cap]
	}
	return n, nil
}

// Bytes returns the captured bytes (the buffer's own slice; do not retain across
// further writes). Caller may pass it to dumpUpstreamResponse.
func (b *boundedBuffer) Bytes() []byte {
	return b.buf
}
