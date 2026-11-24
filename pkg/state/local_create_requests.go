package state

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Decoded create request JSON, an AttributeError as dict.pop raises when it is not an object
func decodeCreateRequest(requestJSON string) (*jsonutil.Object, error) {
	decoded, err := jsonutil.Decode(requestJSON)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	request, ok := decoded.(*jsonutil.Object)
	if !ok {
		return nil, &pyConvError{"AttributeError", fmt.Sprintf("the stored create request is %s, expected a mapping", errchain.Kind(decoded))}
	}
	return request, nil
}

// Keys a for loop over the derived provenance yields, refusing what Python could not iterate or hash
func derivedKeys(derived any) ([]string, error) {
	switch d := derived.(type) {
	case []any:
		var keys []string
		for _, item := range d {
			switch k := item.(type) {
			case string:
				keys = append(keys, k)
			case []any, *jsonutil.Object:
				return nil, &pyConvError{"TypeError", fmt.Sprintf("a derived key of the create request is %s, expected text", errchain.Kind(k))}
			}
		}
		return keys, nil
	case *jsonutil.Object:
		return append([]string(nil), d.Keys()...), nil
	case string:
		var keys []string
		for _, r := range d {
			keys = append(keys, string(r))
		}
		return keys, nil
	}
	return nil, &pyConvError{"TypeError", fmt.Sprintf("the derived keys of the create request are %s, expected a list", errchain.Kind(derived))}
}

// The frozen create request as the client sent it, a nil derived meaning a receipt from before route_derived
func ClientRequest(requestJSON string, derived any) (*jsonutil.Object, error) {
	request, err := decodeCreateRequest(requestJSON)
	if err != nil {
		return nil, err
	}
	request.Take("desktop_local")
	if derived == nil {
		request.Take("route_derived")
	}
	if _, has := request.Get("model_requested"); has {
		request.Take("model")
		keys := []string{"base_url"}
		if derived != nil {
			if keys, err = derivedKeys(derived); err != nil {
				return nil, err
			}
		}
		for _, k := range keys {
			request.Take(k)
		}
	}
	return request, nil
}

// Whether a create retry carries the frozen request it resumes, comparing a pre alias receipt exactly
func SameCreateRequest(savedJSON, retryJSON string) (bool, error) {
	saved, err := decodeCreateRequest(savedJSON)
	if err != nil {
		return false, err
	}
	derived, _ := saved.Get("route_derived")
	savedClient, err := ClientRequest(savedJSON, derived)
	if err != nil {
		return false, err
	}
	retryClient, err := ClientRequest(retryJSON, derived)
	if err != nil {
		return false, err
	}
	if anyutil.Equal(savedClient, retryClient) {
		return true, nil
	}
	retry, err := decodeCreateRequest(retryJSON)
	if err != nil {
		return false, err
	}
	requested, _ := retry.Get("model_requested")
	model, _ := retry.Get("model")
	if anyutil.Equal(requested, model) {
		retry.Take("model_requested")
		retry.Take("route_derived")
	}
	return anyutil.Equal(saved, retry), nil
}

// The exact retry identity of a create, its sent request and its unstored client env judged by the saved policy rules
func SameCreateRetry(savedPolicy *jsonutil.Object, retryJSON string, clientRequest *jsonutil.Object) (bool, error) {
	raw, has := savedPolicy.Get("request_json")
	if !has {
		return false, &pyConvError{"KeyError", "the create request row has no request_json"}
	}
	savedJSON, ok := raw.(string)
	if !ok {
		return false, &pyConvError{"TypeError", fmt.Sprintf("the stored create request is %s, expected JSON text", errchain.Kind(raw))}
	}
	same, err := SameCreateRequest(savedJSON, retryJSON)
	if err != nil || !same {
		return false, err
	}
	return RetryMatches(savedPolicy, clientRequest)
}
