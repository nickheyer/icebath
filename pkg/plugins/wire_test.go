package plugins

import (
	"context"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

type wireProbeRecord struct {
	ToolsetID string
	Count     int
	Hidden    func()
}

func TestWireEncodingsRoundTrip(t *testing.T) {
	encoded := wireEncode(map[string]any{
		"bytes": []byte("hi"), "rec": wireProbeRecord{ToolsetID: "x", Count: 2}, "opaque": &Opaque{TypeName: "Runner", Text: "r"},
		"list": []string{"a"}, "n": 3, "f": 1.5, "none": nil,
	}, nil).(map[string]any)
	if encoded["bytes"].(map[string]any)["__bytes__"] != "aGk=" {
		t.Fatalf("bytes: %#v", encoded["bytes"])
	}
	rec := encoded["rec"].(map[string]any)
	if rec["__record__"] != "wireProbeRecord" || rec["fields"].(map[string]any)["toolset_id"] != "x" {
		t.Fatalf("record: %#v", rec)
	}
	decoded, err := wireDecode(encoded, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decoded.(map[string]any)
	if string(m["bytes"].([]byte)) != "hi" || m["rec"].(Record)["count"] != int64(2) || m["opaque"].(*Opaque).TypeName != "Runner" {
		t.Fatalf("decoded: %#v", m)
	}
	if !reflect.DeepEqual(m["list"], []any{"a"}) || m["n"] != int64(3) || m["f"] != 1.5 || m["none"] != nil {
		t.Fatalf("scalars: %#v", m)
	}
	ref := map[string]any{"__callable__": int64(1)}
	got, _ := wireDecode([]any{ref}, func(r map[string]any) (any, error) { return "proxy", nil })
	if got.([]any)[0] != "proxy" {
		t.Fatalf("resolve: %#v", got)
	}
	unresolved, _ := wireDecode(ref, nil)
	if !reflect.DeepEqual(unresolved, ref) {
		t.Fatalf("ref without resolver: %#v", unresolved)
	}
	opaque := wireEncode(struct{ ch chan int }{}, nil).(map[string]any)
	if _, ok := opaque["__record__"]; !ok {
		t.Fatalf("unexported struct: %#v", opaque)
	}
	if fn := wireEncode(func() {}, nil).(map[string]any); fn["__opaque__"] == nil {
		t.Fatalf("func: %#v", fn)
	}
}

func TestPyJSONParity(t *testing.T) {
	out, err := pyJSONDumps(map[string]any{"a": math.NaN(), "b": math.Inf(-1), "c": "é\n\U0001F600", "d": 1.0, "e": 1e20, "f": []any{int64(1), nil, true}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":NaN,"b":-Infinity,"c":"\u00e9\n\ud83d\ude00","d":1.0,"e":1e+20,"f":[1,null,true]}`
	if string(out) != want {
		t.Fatalf("dumps %s", out)
	}
	back, err := pyJSONLoads(out)
	if err != nil {
		t.Fatal(err)
	}
	m := back.(map[string]any)
	if !math.IsNaN(m["a"].(float64)) || m["c"] != "é\n\U0001F600" || m["d"] != 1.0 || m["f"].([]any)[0] != int64(1) {
		t.Fatalf("loads %#v", m)
	}
	lone, err := pyJSONLoads([]byte(`"\ud800x"`))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := pyJSONDumps(lone)
	if string(again) != `"\ud800x"` {
		t.Fatalf("lone surrogate %s", again)
	}
	if _, err := pyJSONLoads([]byte(`{"a":`)); err == nil {
		t.Fatal("malformed accepted")
	}
}

func TestSignatureKwargsAndRemoteErrors(t *testing.T) {
	sig := signatureFrom([]any{[]any{"tool_name", int64(1), true}, []any{"args", int64(3), true}})
	got := signatureKwargs(sig, map[string]any{"tool_name": "x", "args": 1, "extra": 2})
	if len(got) != 2 || got["extra"] != nil {
		t.Fatalf("narrow: %#v", got)
	}
	all := signatureKwargs(signatureFrom([]any{[]any{"kw", int64(4), false}}), map[string]any{"a": 1})
	if all["a"] != 1 {
		t.Fatalf("var keyword: %#v", all)
	}
	var builtin *HostBuiltinError
	if err := raiseRemote(map[string]any{"type": "ValueError", "message": "bad"}); !errors.As(err, &builtin) || err.Error() != "bad" {
		t.Fatalf("builtin: %v", err)
	}
	var unsupported *PluginHostUnsupported
	if err := raiseRemote(map[string]any{"type": "PluginHostUnsupported", "message": "no"}); !errors.As(err, &unsupported) {
		t.Fatalf("unsupported: %v", err)
	}
	if err := raiseRemote(map[string]any{"type": "SystemExit", "message": "3"}); err.Error() != "SystemExit: 3" {
		t.Fatalf("host error: %v", err)
	}
}

// Two channels joined by pipes, each serving with its own handler
func channelPair(t *testing.T, left, right wireHandler) (*Channel, *Channel) {
	lr, rw := io.Pipe()
	rr, lw := io.Pipe()
	var a, b *Channel
	a = newChannel(lr, lw, left, "left", nil, func(origin any) context.Context { return a.contextOf(origin) })
	b = newChannel(rr, rw, right, "right", nil, nil)
	a.Start()
	b.Start()
	t.Cleanup(func() { a.Close("done"); b.Close("done") })
	return a, b
}

type callerKey struct{}

func TestChannelNestedCallRunsInTheCallerContext(t *testing.T) {
	var right *Channel
	left, r := channelPair(t,
		func(ctx context.Context, method string, params map[string]any, origin any) (any, error) {
			if method == "boom" {
				panic("exit")
			}
			return ctx.Value(callerKey{}), nil
		},
		func(ctx context.Context, method string, params map[string]any, origin any) (any, error) {
			if method == "fail" {
				return nil, &HostBuiltinError{Type: "KeyError", Message: "k"}
			}
			return right.Call(ctx, "whoami", nil, 5*time.Second)
		})
	right = r
	ctx := context.WithValue(context.Background(), callerKey{}, "session-1")
	got, err := left.Call(ctx, "nested", map[string]any{}, 5*time.Second)
	if err != nil || got != "session-1" {
		t.Fatalf("nested origin: %v %v", got, err)
	}
	var builtin *HostBuiltinError
	if _, err := left.Call(ctx, "fail", nil, 5*time.Second); !errors.As(err, &builtin) || builtin.Type != "KeyError" {
		t.Fatalf("remote error: %v", err)
	}
	if _, err := right.Call(ctx, "boom", nil, 5*time.Second); err == nil || !strings.Contains(err.Error(), "SystemExit") {
		t.Fatalf("panic reply: %v", err)
	}
	left.Close("closed by test")
	var un *PluginHostUnavailable
	if _, err := left.Call(ctx, "x", nil, time.Second); !errors.As(err, &un) {
		t.Fatalf("closed channel: %v", err)
	}
}

func TestBindPyMatchesPythonTypeErrors(t *testing.T) {
	params := []pyParam{req("name"), req("handler"), opt("description", ""), kwReq("title")}
	cases := map[string]struct {
		args   []any
		kwargs map[string]any
	}{
		"PluginContext.f() missing 1 required positional argument: 'handler'":       {[]any{"n"}, map[string]any{"title": "t"}},
		"PluginContext.f() takes from 3 to 4 positional arguments but 5 were given": {[]any{1, 2, 3, 4}, nil},
		"PluginContext.f() got an unexpected keyword argument 'zz'":                 {[]any{1, 2}, map[string]any{"zz": 1, "title": 1}},
		"PluginContext.f() got multiple values for argument 'name'":                 {[]any{1, 2}, map[string]any{"name": 1}},
		"PluginContext.f() missing 1 required keyword-only argument: 'title'":       {[]any{1, 2}, nil},
	}
	for want, c := range cases {
		if _, _, err := bindPy("f", params, c.args, c.kwargs); err == nil || err.Error() != want {
			t.Fatalf("want %q got %v", want, err)
		}
	}
	bound, _, err := bindPy("f", params, []any{1, 2}, map[string]any{"title": "t"})
	if err != nil || bound["description"] != "" || bound["title"] != "t" {
		t.Fatalf("bound %#v %v", bound, err)
	}
}
