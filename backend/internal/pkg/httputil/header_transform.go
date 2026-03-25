package httputil

import (
	"bytes"
	"net"
	"strings"
	"sync"
)

// claudeHeaderOrder defines the header order matching real Claude Code traffic
// captured via eBPF analysis of Claude CLI v2.1.39 (Bun + BoringSSL).
var claudeHeaderOrder = []string{
	"accept",
	"authorization",
	"content-type",
	"content-length",
	"user-agent",
	"x-stainless-arch",
	"x-stainless-helper-method",
	"x-stainless-lang",
	"x-stainless-os",
	"x-stainless-package-version",
	"x-stainless-retry-count",
	"x-stainless-runtime",
	"x-stainless-runtime-version",
	"x-stainless-timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-api-key",
	"x-app",
	"host",
	"accept-encoding",
	"connection",
	"transfer-encoding",
}

var claudeHeaderOrderIndex map[string]int

func init() {
	claudeHeaderOrderIndex = make(map[string]int, len(claudeHeaderOrder))
	for i, h := range claudeHeaderOrder {
		claudeHeaderOrderIndex[h] = i
	}
}

var httpMethods = [][]byte{
	[]byte("GET "), []byte("POST "), []byte("PUT "),
	[]byte("DELETE "), []byte("PATCH "), []byte("HEAD "), []byte("OPTIONS "),
}

type headerEntry struct {
	name  string // lowercase name for ordering
	raw   string // "name: value" with lowercase name
	order int    // predefined order index
}

func looksLikeHTTPRequest(data []byte) bool {
	for _, m := range httpMethods {
		if bytes.HasPrefix(data, m) {
			return true
		}
	}
	return false
}

// TransformHTTPHeaders lowercases header names and reorders them to match
// real Claude Code traffic patterns. The transformation is applied in-place
// where possible. Returns a new byte slice with transformed headers.
//
// Only transforms the header section (up to \r\n\r\n). Body data is
// preserved unchanged.
func TransformHTTPHeaders(data []byte) []byte {
	if !looksLikeHTTPRequest(data) {
		return data
	}

	headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return data
	}

	headerSection := data[:headerEnd]
	bodySection := data[headerEnd:] // includes \r\n\r\n

	// Split into request line and header lines
	firstLineEnd := bytes.Index(headerSection, []byte("\r\n"))
	if firstLineEnd < 0 {
		return data
	}

	requestLine := headerSection[:firstLineEnd]
	headerLines := headerSection[firstLineEnd+2:]

	var headers []headerEntry
	for _, line := range bytes.Split(headerLines, []byte("\r\n")) {
		if len(line) == 0 {
			continue
		}
		colonIdx := bytes.IndexByte(line, ':')
		if colonIdx < 0 {
			headers = append(headers, headerEntry{
				name:  strings.ToLower(string(line)),
				raw:   string(line),
				order: len(claudeHeaderOrder),
			})
			continue
		}

		name := strings.ToLower(string(line[:colonIdx]))
		value := string(line[colonIdx:]) // includes ": value"
		raw := name + value

		idx, ok := claudeHeaderOrderIndex[name]
		if !ok {
			idx = len(claudeHeaderOrder)
		}

		headers = append(headers, headerEntry{
			name:  name,
			raw:   raw,
			order: idx,
		})
	}

	// Stable sort by predefined order
	sortHeaders(headers)

	// Rebuild header section
	var buf bytes.Buffer
	buf.Grow(len(data) + 64)
	buf.Write(requestLine)
	buf.WriteString("\r\n")

	for _, h := range headers {
		buf.WriteString(h.raw)
		buf.WriteString("\r\n")
	}

	buf.Write(bodySection)

	return buf.Bytes()
}

// sortHeaders performs a stable insertion sort by the order field.
func sortHeaders(headers []headerEntry) {
	for i := 1; i < len(headers); i++ {
		key := headers[i]
		j := i - 1
		for j >= 0 && headers[j].order > key.order {
			headers[j+1] = headers[j]
			j--
		}
		headers[j+1] = key
	}
}

// LowercaseHeaderConn wraps a net.Conn and transforms outgoing HTTP
// headers to lowercase with Claude Code-matching order.
//
// It detects HTTP request headers in Write calls and transforms them
// before writing to the underlying connection. Non-header writes
// (body data, non-HTTP data) pass through unchanged.
//
// This works because Go's net/http uses a bufio.Writer that flushes
// all headers in a single Write call to the underlying connection.
type LowercaseHeaderConn struct {
	net.Conn
	mu  sync.Mutex
	buf []byte
}

// WrapConn creates a new LowercaseHeaderConn wrapping the given connection.
func WrapConn(conn net.Conn) net.Conn {
	return &LowercaseHeaderConn{Conn: conn}
}

func (c *LowercaseHeaderConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	original := len(p)

	// If we have buffered data from a previous partial write, append
	if len(c.buf) > 0 {
		c.buf = append(c.buf, p...)
		data := c.buf

		if !looksLikeHTTPRequest(data) {
			c.buf = nil
			_, err := c.Conn.Write(data)
			if err != nil {
				return 0, err
			}
			return original, nil
		}

		headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			// Still no complete headers; keep buffering (up to 16KB safety limit)
			if len(c.buf) > 16384 {
				c.buf = nil
				_, err := c.Conn.Write(data)
				if err != nil {
					return 0, err
				}
			}
			return original, nil
		}

		transformed := TransformHTTPHeaders(data)
		c.buf = nil
		_, err := c.Conn.Write(transformed)
		if err != nil {
			return 0, err
		}
		return original, nil
	}

	// Fast path: not an HTTP request, pass through
	if !looksLikeHTTPRequest(p) {
		return c.Conn.Write(p)
	}

	// Check if we have complete headers
	headerEnd := bytes.Index(p, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		// Incomplete headers, buffer for later
		c.buf = make([]byte, len(p))
		copy(c.buf, p)
		return original, nil
	}

	transformed := TransformHTTPHeaders(p)
	_, err := c.Conn.Write(transformed)
	if err != nil {
		return 0, err
	}
	return original, nil
}
