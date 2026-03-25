package httputil

import (
	"bytes"
	"net"
	"strings"
	"testing"
)

func TestTransformHTTPHeaders_Lowercase(t *testing.T) {
	raw := "POST /v1/messages HTTP/1.1\r\n" +
		"Host: api.anthropic.com\r\n" +
		"Content-Type: application/json\r\n" +
		"User-Agent: claude-cli/2.1.39\r\n" +
		"Accept: application/json\r\n" +
		"\r\n"

	result := TransformHTTPHeaders([]byte(raw))
	resultStr := string(result)

	if strings.Contains(resultStr, "Content-Type:") {
		t.Errorf("header name should be lowercased, got Content-Type")
	}
	if !strings.Contains(resultStr, "content-type: application/json") {
		t.Errorf("expected lowercase content-type, got:\n%s", resultStr)
	}
	if !strings.Contains(resultStr, "user-agent: claude-cli/2.1.39") {
		t.Errorf("expected lowercase user-agent, got:\n%s", resultStr)
	}
	if !strings.Contains(resultStr, "host: api.anthropic.com") {
		t.Errorf("expected lowercase host, got:\n%s", resultStr)
	}
	if !strings.HasPrefix(resultStr, "POST /v1/messages HTTP/1.1\r\n") {
		t.Errorf("request line should be unchanged")
	}
	if !strings.HasSuffix(resultStr, "\r\n\r\n") {
		t.Errorf("should end with CRLFCRLF")
	}
}

func TestTransformHTTPHeaders_Reorder(t *testing.T) {
	// Go's net/http puts Host first; real Claude Code puts it near the end
	raw := "POST /v1/messages HTTP/1.1\r\n" +
		"Host: api.anthropic.com\r\n" +
		"X-App: cli\r\n" +
		"Accept: application/json\r\n" +
		"Authorization: Bearer sk-test\r\n" +
		"Content-Type: application/json\r\n" +
		"User-Agent: claude-cli/2.1.39\r\n" +
		"\r\n"

	result := TransformHTTPHeaders([]byte(raw))
	lines := strings.Split(string(result), "\r\n")

	// Find positions of key headers
	positions := map[string]int{}
	for i, line := range lines {
		if idx := strings.IndexByte(line, ':'); idx > 0 {
			positions[line[:idx]] = i
		}
	}

	// accept should come before authorization
	if positions["accept"] >= positions["authorization"] {
		t.Errorf("accept (pos %d) should come before authorization (pos %d)", positions["accept"], positions["authorization"])
	}
	// authorization should come before content-type
	if positions["authorization"] >= positions["content-type"] {
		t.Errorf("authorization (pos %d) should come before content-type (pos %d)", positions["authorization"], positions["content-type"])
	}
	// host should come after x-app (near the end)
	if positions["host"] <= positions["x-app"] {
		t.Errorf("host (pos %d) should come after x-app (pos %d)", positions["host"], positions["x-app"])
	}
}

func TestTransformHTTPHeaders_PreservesBody(t *testing.T) {
	body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	raw := "POST /v1/messages HTTP/1.1\r\n" +
		"Content-Type: application/json\r\n" +
		"Host: api.anthropic.com\r\n" +
		"\r\n" + body

	result := TransformHTTPHeaders([]byte(raw))

	if !strings.HasSuffix(string(result), body) {
		t.Errorf("body should be preserved unchanged")
	}
}

func TestTransformHTTPHeaders_NonHTTPPassthrough(t *testing.T) {
	data := []byte("this is not an HTTP request")
	result := TransformHTTPHeaders(data)
	if !bytes.Equal(result, data) {
		t.Errorf("non-HTTP data should pass through unchanged")
	}
}

func TestTransformHTTPHeaders_NoHeaderEndPassthrough(t *testing.T) {
	data := []byte("GET /path HTTP/1.1\r\nHost: example.com\r\n")
	result := TransformHTTPHeaders(data)
	if !bytes.Equal(result, data) {
		t.Errorf("incomplete headers should pass through unchanged")
	}
}

func TestLowercaseHeaderConn_Write(t *testing.T) {
	// Create a pipe to capture what the conn wrapper writes
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	wrapped := WrapConn(clientConn)

	raw := "POST /v1/messages HTTP/1.1\r\n" +
		"Host: api.anthropic.com\r\n" +
		"Content-Type: application/json\r\n" +
		"Accept: application/json\r\n" +
		"\r\n"

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := serverConn.Read(buf)
		done <- buf[:n]
	}()

	n, err := wrapped.Write([]byte(raw))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(raw) {
		t.Errorf("Write returned %d, want %d", n, len(raw))
	}

	received := <-done
	receivedStr := string(received)

	if strings.Contains(receivedStr, "Content-Type:") {
		t.Errorf("wrapped conn should lowercase headers, got:\n%s", receivedStr)
	}
	if !strings.Contains(receivedStr, "content-type: application/json") {
		t.Errorf("expected lowercase content-type in output:\n%s", receivedStr)
	}
}

func TestLowercaseHeaderConn_NonHTTPPassthrough(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	wrapped := WrapConn(clientConn)

	data := []byte("some binary data that is not HTTP")

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := serverConn.Read(buf)
		done <- buf[:n]
	}()

	n, err := wrapped.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write returned %d, want %d", n, len(data))
	}

	received := <-done
	if !bytes.Equal(received, data) {
		t.Errorf("non-HTTP data should pass through unchanged")
	}
}
