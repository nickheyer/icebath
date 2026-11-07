package state

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Decoded create request JSON, an AttributeError as dict.pop raises when it is not an object
func decodeCreateRequest(requestJSON string) (*pyjson.Object, error) {
	decoded, err := pyjson.Loads(requestJSON)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	request, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, &pyConvError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'pop'", pyTypeName(decoded))}
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
			case []any, *pyjson.Object:
				return nil, &pyConvError{"TypeError", fmt.Sprintf("unhashable type: '%s'", pyTypeName(k))}
			}
		}
		return keys, nil
	case *pyjson.Object:
		return append([]string(nil), d.Keys...), nil
	case string:
		var keys []string
		for _, r := range d {
			keys = append(keys, string(r))
		}
		return keys, nil
	}
	return nil, &pyConvError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyTypeName(derived))}
}

// The frozen create request as the client sent it, a nil derived meaning a receipt from before route_derived
func ClientRequest(requestJSON string, derived any) (*pyjson.Object, error) {
	request, err := decodeCreateRequest(requestJSON)
	if err != nil {
		return nil, err
	}
	request.Pop("desktop_local")
	if derived == nil {
		request.Pop("route_derived")
	}
	if _, has := request.Get("model_requested"); has {
		request.Pop("model")
		keys := []string{"base_url"}
		if derived != nil {
			if keys, err = derivedKeys(derived); err != nil {
				return nil, err
			}
		}
		for _, k := range keys {
			request.Pop(k)
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
	if pyjson.Equal(savedClient, retryClient) {
		return true, nil
	}
	retry, err := decodeCreateRequest(retryJSON)
	if err != nil {
		return false, err
	}
	requested, _ := retry.Get("model_requested")
	model, _ := retry.Get("model")
	if pyjson.Equal(requested, model) {
		retry.Pop("model_requested")
		retry.Pop("route_derived")
	}
	return pyjson.Equal(saved, retry), nil
}

// The exact retry identity of a create, its sent request and its unstored client env judged by the saved policy rules
func SameCreateRetry(savedPolicy *pyjson.Object, retryJSON string, clientRequest *pyjson.Object) (bool, error) {
	raw, has := savedPolicy.Get("request_json")
	if !has {
		return false, &pyConvError{"KeyError", "'request_json'"}
	}
	savedJSON, ok := raw.(string)
	if !ok {
		return false, &pyConvError{"TypeError", fmt.Sprintf("the JSON object must be str, bytes or bytearray, not %s", pyTypeName(raw))}
	}
	same, err := SameCreateRequest(savedJSON, retryJSON)
	if err != nil || !same {
		return false, err
	}
	return RetryMatches(savedPolicy, clientRequest)
}
