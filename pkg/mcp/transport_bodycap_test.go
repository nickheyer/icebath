package mcp

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

const capTestLimit = 1024

type chunkBody struct{ chunks [][]byte }

func (c *chunkBody) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	if n == len(c.chunks[0]) {
		c.chunks = c.chunks[1:]
	} else {
		c.chunks[0] = c.chunks[0][n:]
	}
	return n, nil
}

func (c *chunkBody) Close() error { return nil }

type mockRoundTripper func(*http.Request) *http.Response

func (m mockRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return m(r), nil }

// cappedClient routes requests through the body cap over a handler returning the given response shape
func cappedClient(limit int, contentType string, contentLength string, chunks ...[]byte) *http.Client {
	inner := mockRoundTripper(func(r *http.Request) *http.Response {
		h := http.Header{}
		if contentType != "" {
			h.Set("Content-Type", contentType)
		}
		if contentLength != "" {
			h.Set("Content-Length", contentLength)
		}
		return &http.Response{StatusCode: 200, Header: h, Body: &chunkBody{chunks: append([][]byte(nil), chunks...)}, Request: r}
	})
	return &http.Client{Transport: &bodyCapTransport{inner: inner, limit: limit}}
}

func drainCapped(t *testing.T, c *http.Client, url string) ([]byte, error) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func expectCapError(t *testing.T, err error, needle string) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrBodyTooLarge) || !strings.Contains(err.Error(), needle) {
		t.Fatalf("want body cap error containing %q, got %v", needle, err)
	}
}

func TestBodyCapSmallJSONBodyPasses(t *testing.T) {
	body := []byte(`{"ok": true}`)
	got, err := drainCapped(t, cappedClient(capTestLimit, "application/json", fmt.Sprint(len(body)), body), "http://mcp.test/rpc")
	if err != nil || string(got) != `{"ok": true}` {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestBodyCapOversizedBodyRejectedViaContentLength(t *testing.T) {
	body := []byte(strings.Repeat("x", capTestLimit+1))
	_, err := drainCapped(t, cappedClient(capTestLimit, "", fmt.Sprint(len(body)), body), "http://mcp.test/rpc")
	expectCapError(t, err, "Content-Length")
}

func TestBodyCapOversizedStreamedBodyRejectedWithoutContentLength(t *testing.T) {
	var chunks [][]byte
	for range 8 {
		chunks = append(chunks, []byte(strings.Repeat("y", capTestLimit/4)))
	}
	_, err := drainCapped(t, cappedClient(capTestLimit, "", "", chunks...), "http://mcp.test/rpc")
	expectCapError(t, err, "HTTP response exceeds")
}

func TestBodyCapSSEEventOverCapRejected(t *testing.T) {
	_, err := drainCapped(t, cappedClient(capTestLimit, "text/event-stream", "", []byte("data: "+strings.Repeat("z", capTestLimit+64))), "http://mcp.test/sse")
	expectCapError(t, err, "SSE event exceeds")
}

func TestBodyCapSSECumulativeKeepalivesUnlimited(t *testing.T) {
	var chunks [][]byte
	for i := range 64 {
		chunks = append(chunks, []byte(fmt.Sprintf(": keepalive %d\n\ndata: {\"n\": %d}\n\n", i, i)))
	}
	got, err := drainCapped(t, cappedClient(64, "text/event-stream", "", chunks...), "http://mcp.test/sse")
	if err != nil || len(got) <= 64 {
		t.Fatalf("cumulative keepalives must pass, got %d bytes %v", len(got), err)
	}
}

func TestBodyCapSSEEventSplitAcrossChunksCountsPrefix(t *testing.T) {
	var chunks [][]byte
	for range 6 {
		chunks = append(chunks, []byte("data: "+strings.Repeat("q", capTestLimit/4)))
	}
	_, err := drainCapped(t, cappedClient(capTestLimit, "text/event-stream", "", chunks...), "http://mcp.test/sse")
	expectCapError(t, err, "SSE event exceeds")
}

var (
	capEvent1 = "data: " + strings.Repeat("a", 40)
	capEvent2 = "data: " + strings.Repeat("b", 40)
)

func TestBodyCapSSEBoundarySplitAcrossChunksStillResets(t *testing.T) {
	type tc struct {
		id     string
		chunks []string
	}
	var cases []tc
	for _, sep := range []string{"\n\n", "\r\r", "\n\r", "\r\n\r\n", "\r\n\n", "\r\n\r", "\n\r\n", "\r\r\n"} {
		for cut := 1; cut < len(sep); cut++ {
			cases = append(cases, tc{fmt.Sprintf("%q@%d", sep, cut), []string{capEvent1 + sep[:cut], sep[cut:] + capEvent2 + sep}})
		}
	}
	cases = append(cases,
		tc{"three-way", []string{capEvent1 + "\r", "\n\r", "\n" + capEvent2 + "\r\n\r\n"}},
		tc{"two-events-one-chunk", []string{capEvent1 + "\n\n" + capEvent2 + "\n\n"}})
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			var chunks [][]byte
			for _, s := range c.chunks {
				chunks = append(chunks, []byte(s))
			}
			if _, err := drainCapped(t, cappedClient(64, "text/event-stream", "", chunks...), "http://mcp.test/sse"); err != nil {
				t.Fatalf("each event fits the cap, got %v", err)
			}
		})
	}
}

func TestBodyCapSSECRLFSplitAcrossChunksInsideEventIsNotABoundary(t *testing.T) {
	_, err := drainCapped(t, cappedClient(64, "text/event-stream", "", []byte(capEvent1+"\r"), []byte("\n"+capEvent2+"\r\n\r\n")), "http://mcp.test/sse")
	expectCapError(t, err, "SSE event exceeds")
}

func TestSSEReaderSplitsEventsOnEveryLineTerminator(t *testing.T) {
	for _, sep := range []string{"\n\n", "\r\r", "\n\r", "\r\n\r\n", "\r\n\n", "\r\n\r", "\n\r\n", "\r\r\n"} {
		r := NewSSEReader(strings.NewReader(capEvent1+sep+"event: x\r"+capEvent2+sep), 64, "test")
		first, err := r.Next()
		if err != nil || first.Data != strings.Repeat("a", 40) {
			t.Fatalf("%q first event %+v %v", sep, first, err)
		}
		second, err := r.Next()
		if err != nil || second.Data != strings.Repeat("b", 40) || second.Event != "x" {
			t.Fatalf("%q second event %+v %v", sep, second, err)
		}
	}
}
