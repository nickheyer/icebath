package pyhttpx

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestRaiseForStatusMessageMatchesHTTPX(t *testing.T) {
	got := RaiseForStatusMessage(404, "http://h/x.png", "")
	want := "Client error '404 Not Found' for url 'http://h/x.png'\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/404"
	if got != want {
		t.Fatalf("got %q", got)
	}
	got = RaiseForStatusMessage(302, "http://h/", "/next")
	want = "Redirect response '302 Found' for url 'http://h/'\nRedirect location: '/next'\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/302"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if got := RaiseForStatusMessage(503, "u", ""); got[:33] != "Server error '503 Service Unavail" {
		t.Fatalf("got %q", got)
	}
}

func TestRaiseForStatusNamesTheLocationOnlyForFollowedRedirects(t *testing.T) {
	for _, status := range []int{300, 304, 305} {
		got := RaiseForStatusMessage(status, "http://h/", "/next")
		want := fmt.Sprintf("Redirect response '%d %s' for url 'http://h/'\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/%d", status, http.StatusText(status), status)
		if got != want {
			t.Fatalf("status %d got %q", status, got)
		}
	}
	for _, status := range []int{301, 302, 303, 307, 308} {
		if got := RaiseForStatusMessage(status, "http://h/", "/next"); !strings.Contains(got, "\nRedirect location: '/next'\n") {
			t.Fatalf("status %d got %q", status, got)
		}
	}
}

func TestStatusErrorTextUsesTheReasonPhraseOfTheStatusLine(t *testing.T) {
	r := &Response{StatusCode: 404, Status: "404 Nope", Header: http.Header{}}
	if got, want := r.StatusErrorText("http://h/x"), "Client error '404 Nope' for url 'http://h/x'\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/404"; got != want {
		t.Fatalf("got %q", got)
	}
	r = &Response{StatusCode: 404, Status: "404", Header: http.Header{}}
	if got := r.StatusErrorText("u"); !strings.HasPrefix(got, "Client error '404 ' for url 'u'") {
		t.Fatalf("empty reason got %q", got)
	}
	r = &Response{StatusCode: 503, Header: http.Header{}}
	if got := r.StatusErrorText("u"); !strings.HasPrefix(got, "Server error '503 Service Unavailable' for url 'u'") {
		t.Fatalf("unread response got %q", got)
	}
	r = &Response{StatusCode: 302, Status: "302 Found", Header: http.Header{"Location": {""}}}
	if got := r.StatusErrorText("u"); !strings.Contains(got, "\nRedirect location: ''\n") {
		t.Fatalf("empty location got %q", got)
	}
	r = &Response{StatusCode: 302, Status: "302 Found", Header: http.Header{}}
	if got := r.StatusErrorText("u"); strings.Contains(got, "Redirect location") {
		t.Fatalf("absent location got %q", got)
	}
}
