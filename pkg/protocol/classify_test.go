package protocol

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		e             Error
		billing, auth bool
	}{
		{Error{Status: 400, Type: "invalid_request_error", Message: "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}, true, false},
		{Error{Status: 429, Type: "insufficient_quota", Message: "You exceeded your current quota, please check your plan and billing details.", Transient: true}, true, false},
		{Error{Status: 429, Type: "RESOURCE_EXHAUSTED", Message: "Quota exceeded for quota metric 'Generate Content API requests per minute'", Transient: true}, false, false},
		{Error{Status: 402, Message: "payment required"}, true, false},
		{Error{Status: 401, Type: "authentication_error", Message: "invalid x-api-key"}, false, true},
		{Error{Status: 403, Type: "permission_error", Message: "model not allowed"}, false, false},
		{Error{Status: 529, Type: "overloaded_error", Message: "Overloaded", Transient: true}, false, false},
		{Error{Status: 429, Type: "rate_limit_error", Message: "This organization has reached its monthly spend limit", Transient: true}, true, false},
	}
	for _, c := range cases {
		e := c.e
		Classify(&e)
		if e.Billing != c.billing || e.Auth != c.auth {
			t.Fatalf("%+v: billing %v auth %v", c.e, e.Billing, e.Auth)
		}
		if (e.Billing || e.Auth) && (e.Transient || !e.Stuck() || e.Why() == "") {
			t.Fatalf("%+v still transient or not stuck", e)
		}
		if !e.Billing && !e.Auth && (e.Stuck() || e.Transient != c.e.Transient) {
			t.Fatalf("%+v changed", e)
		}
	}
	wrapped := StuckError(wrap(&Error{Status: 402, Message: "pay"}))
	if wrapped == nil || !wrapped.Billing {
		t.Fatal("stuck error not found through the chain")
	}
	if StuckError(wrap(&Error{Status: 500, Message: "boom", Transient: true})) != nil {
		t.Fatal("transient error taken as stuck")
	}
}

type wrapErr struct{ err error }

func (w wrapErr) Error() string { return "wrapped: " + w.err.Error() }
func (w wrapErr) Unwrap() error { return w.err }

func wrap(e *Error) error { return wrapErr{err: Classify(e)} }
