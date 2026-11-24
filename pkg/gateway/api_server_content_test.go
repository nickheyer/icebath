package gateway

import (
	"context"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func textPart(kind, text string) *jsonutil.Object {
	return jsonutil.ObjectOf("type", kind, "text", text)
}

func TestNoneReturnsEmptyString(t *testing.T) {
	if got := apiNormalizeChatContent(nil); got != "" {
		t.Fatalf("chat %q", got)
	}
	if got, err := apiNormalizeMultimodalContent(context.Background(), nil); err != nil || got != "" {
		t.Fatalf("multimodal %v %v", got, err)
	}
}

func TestPlainStringReturnedAsIs(t *testing.T) {
	if got := apiNormalizeChatContent("hello world"); got != "hello world" {
		t.Fatalf("%q", got)
	}
}

func TestTextContentPart(t *testing.T) {
	if got := apiNormalizeChatContent([]any{textPart("text", "hello")}); got != "hello" {
		t.Fatalf("%q", got)
	}
}

func TestInputTextContentPart(t *testing.T) {
	if got := apiNormalizeChatContent([]any{textPart("input_text", "user input")}); got != "user input" {
		t.Fatalf("%q", got)
	}
}

func TestOutputTextContentPart(t *testing.T) {
	if got := apiNormalizeChatContent([]any{textPart("output_text", "assistant output")}); got != "assistant output" {
		t.Fatalf("%q", got)
	}
}

func TestEmptyTextPartsFiltered(t *testing.T) {
	if got := apiNormalizeChatContent([]any{textPart("text", ""), textPart("text", "actual"), textPart("text", "")}); got != "actual" {
		t.Fatalf("%q", got)
	}
}

func TestStringPassthrough(t *testing.T) {
	if got, err := apiNormalizeMultimodalContent(context.Background(), "hello"); err != nil || got != "hello" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestTextOnlyListCollapsesToString(t *testing.T) {
	if got, err := apiNormalizeMultimodalContent(context.Background(), []any{textPart("text", "hi"), textPart("text", "there")}); err != nil || got != "hi\nthere" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestResponsesInputTextCanonicalized(t *testing.T) {
	if got, err := apiNormalizeMultimodalContent(context.Background(), []any{textPart("input_text", "hello")}); err != nil || got != "hello" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestListWithImageOnly(t *testing.T) {
	if !apiContentHasVisiblePayload([]any{jsonutil.ObjectOf("type", "image_url", "image_url", jsonutil.ObjectOf("url", "x"))}) {
		t.Fatal("image only content has no payload")
	}
}
