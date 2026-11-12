package tuiserver

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestPickOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		answer  string
		elapsed float64
		result  map[string]any
	}{
		{"picked", `{"picked": {"accent": "violet"}}`, 4.0, map[string]any{"success": true, "picked": map[string]any{"accent": "violet"}}},
		{"said", `{"said": " make it pink "}`, 4.0, map[string]any{"said": "make it pink"}},
		{"cancelled", `{"cancelled": true}`, 4.0, map[string]any{"cancelled": true}},
		{"no answer", "", 4.0, map[string]any{"cancelled": true}},
		{"expired", "", themePickTimeoutS, map[string]any{"timed_out": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := []float64{0.0, tc.elapsed}
			type sentCall struct {
				method, sid string
				params      map[string]any
				timeout     float64
			}
			var sent []sentCall
			savedSend, savedClock := themeSend, themeMonotonic
			themeSend = func(ctx context.Context, method, sid string, params *pyjson.Object, timeout *float64, qids []string) (*pyjson.Object, error) {
				sent = append(sent, sentCall{method, sid, objectMap(params), *timeout})
				if tc.answer == "" {
					return nil, nil
				}
				return pyjson.LoadsDict(tc.answer)
			}
			themeMonotonic = func() float64 {
				now := clock[0]
				clock = clock[1:]
				return now
			}
			t.Cleanup(func() { themeSend, themeMonotonic = savedSend, savedClock })
			liveRecord(t, "s1")
			got := themeRequest(context.Background(), "s1", map[string]any{"picker": "accent"}).(map[string]any)
			if picked, isObj := got["picked"].(*pyjson.Object); isObj {
				got["picked"] = objectMap(picked)
			}
			if !reflect.DeepEqual(got, tc.result) {
				t.Fatalf("result = %v want %v", got, tc.result)
			}
			want := []sentCall{{"theme.pick", "s1", map[string]any{"picker": "accent"}, themePickTimeoutS}}
			if !reflect.DeepEqual(sent, want) {
				t.Fatalf("sent = %+v", sent)
			}
		})
	}
}

func TestTheSessionCallbacksReachTheBridge(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fake := installBridge(t)
	fake.answers = []string{`{"success": true}`}
	got := agentCbs(context.Background(), "s1").Theme(map[string]any{"accent": "violet"})
	if !reflect.DeepEqual(objectMap(got.(*pyjson.Object)), map[string]any{"success": true}) {
		t.Fatalf("theme callback = %v", got)
	}
	want := []bridgeCall{{event: "theme.apply", sid: "s1", payload: map[string]any{"accent": "violet"}, timeout: themeTimeoutS}}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls = %+v", fake.calls)
	}
}
