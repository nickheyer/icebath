package googlechat

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestFormatMessageBoldDoubleAsteriskToSingle(t *testing.T) {
	if out := FormatMessage("hello **world**"); out != "hello *world*" {
		t.Fatalf("got %q", out)
	}
}

func TestFormatMessageMarkdownLinkToChatAnglebracket(t *testing.T) {
	if out := FormatMessage("see [docs](https://example.com)"); out != "see <https://example.com|docs>" {
		t.Fatalf("got %q", out)
	}
}

func TestFormatMessageHeaderToBoldAtLineStartOnly(t *testing.T) {
	if out := FormatMessage("# Heading\nbody with # mid-line hash"); out != "*Heading*\nbody with # mid-line hash" {
		t.Fatalf("got %q", out)
	}
}

func TestFormatMessageStripsZWJAndVariationSelector(t *testing.T) {
	zwj := string(rune(0x200d))
	out := FormatMessage("hello \U0001f468" + zwj + "\U0001f469" + zwj + "\U0001f467 world")
	if strings.Contains(out, zwj) {
		t.Fatalf("ZWJ kept in %q", out)
	}
	for _, r := range []string{"\U0001f468", "\U0001f469", "\U0001f467"} {
		if !strings.Contains(out, r) {
			t.Fatalf("%q lost from %q", r, out)
		}
	}
}

func TestFormatMessageProtectsCodeAndNestedSpans(t *testing.T) {
	if out := FormatMessage("```\n**x**  y\n```  a  **b**"); out != "```\n**x**  y\n``` a *b*" {
		t.Fatalf("got %q", out)
	}
	if out := FormatMessage("## Title `c`"); out != "*Title \x00GC0\x00*" {
		t.Fatalf("got %q", out)
	}
}

func TestCardSpecToCardsV2WireShape(t *testing.T) {
	spec, err := pyjson.Loads(`{"card_id": "c1", "header": {"title": "T", "image_url": "u", "image_alt_text": "alt"},
		"sections": [{"header": "S", "widgets": [{"type": "text", "text": "**hi**"},
		{"type": "buttons", "buttons": [{"text": "Go", "action": "act", "parameters": {"z": 1, "a": true}}]},
		{"type": "selection", "name": "n", "items": [{"text": "A", "value": "a", "selected": 1}]},
		{"type": "decorated_text", "text": "d", "top_label": "tl"}, {"type": "divider"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	card, err := CardSpecToCardsV2(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pyjson.Dumps(card)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cardId": "c1", "card": {"sections": [{"widgets": [{"textParagraph": {"text": "*hi*"}}, {"buttonList": {"buttons": [{"text": "Go", "onClick": {"action": {"function": "act", "parameters": [{"key": "a", "value": "True"}, {"key": "z", "value": "1"}]}}}]}}, {"selectionInput": {"name": "n", "label": "n", "type": "CHECK_BOX", "items": [{"text": "A", "value": "a", "selected": true}]}}, {"decoratedText": {"text": "d", "wrapText": true, "topLabel": "tl"}}, {"divider": {}}], "header": "S"}], "header": {"title": "T", "imageUrl": "u", "imageType": "SQUARE", "imageAltText": "alt"}}}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCardSpecToCardsV2Errors(t *testing.T) {
	for spec, msg := range map[string]string{
		`[]`:                                "card must be an object",
		`{"sections": []}`:                  "card.sections must contain at least one section",
		`{"sections": [{"widgets": [{}]}]}`: "unsupported widget type: <missing>",
		`{"sections": [{"widgets": [{"type": "image"}]}]}`:                                "widget.image_url is required",
		`{"sections": [{"widgets": [{"type": "selection", "name": "n", "items": [1]}]}]}`: "selection items must be objects",
	} {
		v, _ := pyjson.Loads(spec)
		if _, err := CardSpecToCardsV2(v); err == nil || err.Error() != msg {
			t.Fatalf("%s: err = %v, want %s", spec, err, msg)
		}
	}
	card, _ := CardSpecToCardsV2(map[string]any{"sections": []any{map[string]any{"widgets": []any{map[string]any{"type": "divider"}}}}})
	if v, _ := card.Get("cardId"); v != "icebath-card" {
		t.Fatalf("cardId = %v", v)
	}
}
