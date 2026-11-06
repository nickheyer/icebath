package pyhttpx

import "testing"

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
