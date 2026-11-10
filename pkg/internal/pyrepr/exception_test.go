package pyrepr

import (
	"errors"
	"fmt"
	"testing"
)

type namedErr struct{}

func (namedErr) Error() string      { return "boom" }
func (namedErr) PyTypeName() string { return "ConfigBackendUnavailable" }

func TestExceptionNameAndMessage(t *testing.T) {
	cases := []struct {
		err        error
		name, text string
	}{
		{errors.New("provider detail"), "Exception", "provider detail"},
		{errors.New("TypeError: 'NoneType' object is not callable"), "TypeError", "'NoneType' object is not callable"},
		{fmt.Errorf("wrap: %w", namedErr{}), "ConfigBackendUnavailable", "wrap: boom"},
		{&FormatError{Class: "ValueError", Msg: "bad spec"}, "ValueError", "bad spec"},
		{errors.New(""), "Exception", ""},
	}
	for _, tc := range cases {
		if got := ExceptionName(tc.err); got != tc.name {
			t.Errorf("ExceptionName(%q) = %q, want %q", tc.err, got, tc.name)
		}
		if got := ExceptionMessage(tc.err); got != tc.text {
			t.Errorf("ExceptionMessage(%q) = %q, want %q", tc.err, got, tc.text)
		}
	}
}

func TestExceptionReprMatchesPython(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, "None"},
		{errors.New("boom"), "Exception('boom')"},
		{errors.New("ValueError: it's bad"), `ValueError("it's bad")`},
		{errors.New(""), "Exception()"},
		{fmt.Errorf("wrapped: %w", errors.New("x")), "Exception('wrapped: x')"},
		{testReprError{}, "<HTTPError 404: 'Not Found'>"},
	} {
		if got := ExceptionRepr(c.err); got != c.want {
			t.Errorf("ExceptionRepr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// Error carrying its own Python repr
type testReprError struct{}

func (testReprError) Error() string  { return "HTTP Error 404: Not Found" }
func (testReprError) PyRepr() string { return "<HTTPError 404: 'Not Found'>" }
