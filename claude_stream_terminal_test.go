package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseEventNames extracts the SSE event names in write order from the bytes a
// handler wrote (lines of the form "event: <name>").
func sseEventNames(t *testing.T, raw []byte) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimSpace(strings.TrimPrefix(line, "event: ")))
		}
	}
	return names
}

// assertTerminalMessageStop asserts the Anthropic SSE terminal contract: the
// last event written to the client must be message_stop so the client never
// hangs waiting for stream completion.
func assertTerminalMessageStop(t *testing.T, rec *httptest.ResponseRecorder, err error) {
	t.Helper()
	names := sseEventNames(t, rec.Body.Bytes())
	if len(names) == 0 {
		t.Fatalf("no SSE events written (err=%v), want at least a terminal message_stop", err)
	}
	if names[len(names)-1] != "message_stop" {
		t.Fatalf("last SSE event = %q, want \"message_stop\"; events written: %v (err=%v)", names[len(names)-1], names, err)
	}
	if !strings.Contains(rec.Body.String(), `"message_stop"`) {
		t.Fatalf("message_stop data payload missing from written bytes:\n%s", rec.Body.String())
	}
}

func streamUpstreamResponse(body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(body),
	}
}

// TestClaudeStreamTerminalOnTruncatedUpstreamBody feeds two complete chunks
// followed by a chunk truncated mid-JSON (upstream connection died mid-write),
// then io.EOF. The client must still receive a terminal message_stop event.
func TestClaudeStreamTerminalOnTruncatedUpstreamBody(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-trunc\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-trunc\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
		// Truncated mid-JSON, no trailing newline: upstream cut off here.
		"data: {\"id\":\"chatcmpl-trunc\",\"object\":\"chat.compl"

	rec := httptest.NewRecorder()
	err := writeClaudeStreamingResponse(rec, streamUpstreamResponse(strings.NewReader(body)), "claude-sonnet-4-20250514")

	names := sseEventNames(t, rec.Body.Bytes())
	foundMessageStart := false
	for _, name := range names {
		if name == "message_start" {
			foundMessageStart = true
			break
		}
	}
	if !foundMessageStart {
		t.Fatalf("message_start never written; events: %v (err=%v)", names, err)
	}
	assertTerminalMessageStop(t, rec, err)
}

// failAfterBodyReader returns its buffered body on the first Read, then fails
// all subsequent reads with io.ErrUnexpectedEOF, simulating an upstream body
// that errors mid-read.
type failAfterBodyReader struct {
	data   []byte
	offset int
	failed bool
}

func (r *failAfterBodyReader) Read(p []byte) (int, error) {
	if r.offset < len(r.data) {
		n := copy(p, r.data[r.offset:])
		r.offset += n
		return n, nil
	}
	if !r.failed {
		r.failed = true
		return 0, io.ErrUnexpectedEOF
	}
	return 0, io.ErrUnexpectedEOF
}

// TestClaudeStreamTerminalOnMidReadError feeds one complete chunk then fails
// the read with io.ErrUnexpectedEOF. The client must still receive a terminal
// message_stop event.
func TestClaudeStreamTerminalOnMidReadError(t *testing.T) {
	reader := &failAfterBodyReader{
		data: []byte("data: {\"id\":\"chatcmpl-err\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"),
	}

	rec := httptest.NewRecorder()
	err := writeClaudeStreamingResponse(rec, streamUpstreamResponse(reader), "claude-sonnet-4-20250514")

	assertTerminalMessageStop(t, rec, err)
}

// TestClaudeStreamTerminalOnCleanEOFWithoutDone is an adjacent-behavior guard:
// complete valid chunks with no finish_reason and no [DONE], ending in a clean
// io.EOF, must still produce a terminal message_stop. This already works via
// the finalize path; the terminal-stream fix must preserve it.
func TestClaudeStreamTerminalOnCleanEOFWithoutDone(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-clean\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-clean\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done text\"},\"finish_reason\":null}]}\n\n"

	rec := httptest.NewRecorder()
	err := writeClaudeStreamingResponse(rec, streamUpstreamResponse(strings.NewReader(body)), "claude-sonnet-4-20250514")

	assertTerminalMessageStop(t, rec, err)
}
