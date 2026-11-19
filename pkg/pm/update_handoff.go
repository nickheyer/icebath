package pm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Hand off file and update flags of the exact fresh checkout command shipped updaters emit
func LegacyPostSwapInvocation(argv []string) (string, []string, bool) {
	if len(argv) == 0 || argv[0] != "update" {
		return "", nil, false
	}
	marker := slices.Index(argv[1:], "--post-swap")
	if marker < 0 {
		return "", nil, false
	}
	marker++
	if marker+2 != len(argv) {
		return "", nil, false
	}
	return argv[marker+1], argv[1:marker], true
}

// Post swap payload of a hand off file, a ValueError when it holds no JSON object
func readHandoff(path string) (*pyjson.Object, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if err := strictUTF8(data); err != nil {
		return nil, &PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	decoded, err := pyjson.Loads(string(data))
	if err != nil {
		return nil, err
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("post-swap hand-off %s is not a JSON object", pypath.PurePathStr(path))}
	}
	return payload, nil
}

// Takeover request of a post swap payload, the older resume and assume yes names mapped onto the new ones
func takeoverRequest(payload *pyjson.Object, argvTail []string) *OrderedObject {
	request := NewOrderedObject()
	for _, key := range payload.Keys {
		request.Set(key, payload.Vals[key])
	}
	setDefault := func(key string, value any) {
		if _, ok := request.Get(key); !ok {
			request.Set(key, value)
		}
	}
	setDefault("windows_resume", payload.GetOr("windows_gateway_resume", nil))
	setDefault("assume_yes", slices.Contains(argvTail, "--yes"))
	setDefault("restart_update", false)
	return request
}

// Finishes a shipped updater's post swap hand off in a takeover child and returns the child's status
func ContinueLegacyPostSwap(ctx context.Context, handoffPath string, argvTail []string) (int, error) {
	payload, err := readHandoff(handoffPath)
	if err != nil {
		return 0, err
	}
	_ = pypath.Unlink(handoffPath)
	code, _, err := runChild(ctx, takeoverRequest(payload, argvTail))
	return code, err
}

// Env var set on the post swap child, its receipt header reads continued and the lock is the parent's
const PostSwapEnv = "ICEBATH_UPDATE_POST_SWAP"

// True inside the process that finishes an update after the checkout swap
func IsPostSwapChild() bool { return os.Getenv(PostSwapEnv) == "1" }
