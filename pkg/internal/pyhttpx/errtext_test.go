package pyhttpx

import (
	"errors"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestErrorTextMatchesHttpxExceptions(t *testing.T) {
	if got := ErrorText(&TimeoutError{Phase: "read"}); got != "timed out" {
		t.Fatalf("timeout %q", got)
	}
	if got := ErrorText(ErrTooManyRedirects); got != "Exceeded maximum allowed redirects." {
		t.Fatalf("redirects %q", got)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	_, err = Do(PlainClient(false), req, 5*time.Second)
	if err == nil {
		t.Fatal("closed port answered")
	}
	want := "[Errno 111] Connection refused"
	if runtime.GOOS == "windows" {
		want = "[WinError 10061] No connection could be made because the target machine actively refused it"
	}
	if got := ErrorText(err); got != want {
		t.Fatalf("refused %q want %q", got, want)
	}
	dns := &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}
	if got := OSErrorText(dns); runtime.GOOS == "linux" && got != "[Errno -2] Name or service not known" {
		t.Fatalf("dns %q", got)
	}
	if got := OSErrorText(errors.New("plain")); got != "plain" {
		t.Fatalf("plain %q", got)
	}
}

func TestProtocolErrorNamesTheScheme(t *testing.T) {
	for raw, want := range map[string]string{
		"https://a.example/x":   "",
		"HTTP://a.example/x":    "",
		"/search":               "Request URL is missing an 'http://' or 'https://' protocol.",
		"localhost:8080/search": "Request URL has an unsupported protocol 'localhost://'.",
		"ftp://a.example":       "Request URL has an unsupported protocol 'ftp://'.",
	} {
		if got := ProtocolError(raw); got != want {
			t.Fatalf("%s: %q want %q", raw, got, want)
		}
	}
	if !strings.HasPrefix(ProtocolError(""), "Request URL is missing") {
		t.Fatal("empty url")
	}
}
