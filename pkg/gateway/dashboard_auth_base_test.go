package gateway

import (
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjwt"
)

func TestClassifierMapsTransportFailureToProviderError(t *testing.T) {
	err := ClassifyJWKSLookupError(&pyjwt.Error{Class: "PyJWKClientConnectionError", Msg: "Fail to fetch data from the url"})
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Msg != "JWKS lookup failed: Fail to fetch data from the url" {
		t.Fatalf("got %#v", err)
	}
}

func TestClassifierMapsUnverifiableTokenToInvalidCode(t *testing.T) {
	for _, exc := range []*pyjwt.Error{
		{Class: "DecodeError", Msg: "Not enough segments"},
		{Class: "PyJWKSetError", Msg: "The JWK Set did not contain any keys"},
		{Class: "InvalidTokenError", Msg: "bad"},
		{Class: "InvalidSignatureError", Msg: "Signature verification failed"},
		{Class: "ExpiredSignatureError", Msg: "Signature has expired"},
	} {
		var ic *InvalidCodeError
		if err := ClassifyJWKSLookupError(exc); !errors.As(err, &ic) || ic.Msg != "token not verifiable by this provider: "+exc.Msg {
			t.Fatalf("%s gave %#v", exc.Class, err)
		}
	}
}

func TestClassifierKeepsBareJwkClientErrorAsProviderFault(t *testing.T) {
	var pe *ProviderError
	if err := ClassifyJWKSLookupError(&pyjwt.Error{Class: "PyJWKClientError", Msg: "weird JWKS shape"}); !errors.As(err, &pe) {
		t.Fatalf("got %#v", err)
	}
	err := ClassifyJWKSLookupError(&pyjwt.Error{Class: "InvalidKeyError", Msg: "bad key"})
	if !errors.As(err, &pe) || pe.Msg != "JWKS lookup failed: InvalidKeyError('bad key')" {
		t.Fatalf("other jwt error %#v", err)
	}
	err = ClassifyJWKSLookupError(errors.New("boom"))
	if !errors.As(err, &pe) || pe.Msg != "JWKS lookup failed: Exception('boom')" {
		t.Fatalf("plain error %#v", err)
	}
}
