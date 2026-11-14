package telegram

import (
	"context"
	"testing"
)

func TestSendDraftFallsBackToPlainTextOnMarkdownv2Error(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	fake.respond = func(method string, params map[string]string) (int, string) {
		if _, formatted := params["parse_mode"]; method == "sendMessageDraft" && formatted {
			return botError(400, "Bad Request: can't parse entities")
		}
		return 0, ""
	}
	result, err := a.SendDraft(context.Background(), "123", 9, "weird _text", nil)
	if err != nil || !result.Success {
		t.Fatalf("result %+v err %v", result, err)
	}
	calls := fake.callsOf("sendMessageDraft")
	if len(calls) != 2 {
		t.Fatalf("calls %d", len(calls))
	}
	if calls[0].params["parse_mode"] != "MarkdownV2" || calls[0].params["text"] != a.FormatMessage("weird _text") {
		t.Fatalf("first draft %+v", calls[0].params)
	}
	if _, formatted := calls[1].params["parse_mode"]; formatted || calls[1].params["text"] != "weird _text" {
		t.Fatalf("plain retry %+v", calls[1].params)
	}
}
