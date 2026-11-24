package googlechat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
)

// Compiled Python pattern, panicking at init on a malformed constant
func mustPyRe(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

var (
	invisibleRe = mustPyRe("[\u200b\u200c\u200d\u200e\u200f\u2060\ufeff\ufe00-\ufe0f\U000e0100-\U000e01ef]")
	fencedRe    = mustPyRe("(```(?:[^\\n]*\\n)?[\\s\\S]*?```)")
	inlineRe    = mustPyRe("(`[^`]+`)")
	headerRe    = mustPyRe(`(?m)^#{1,6}\s+(.+)$`)
	boldItalRe  = mustPyRe(`\*\*\*(.+?)\*\*\*`)
	boldRe      = mustPyRe(`\*\*(.+?)\*\*`)
	linkRe      = mustPyRe(`\[([^\]]+)\]\(([^)]+)\)`)
	spacesRe    = mustPyRe(`  +`)
)

func sub(re *regexp2.Regexp, s string, fn func(regexp2.Match) string) string {
	out, err := re.ReplaceFunc(s, fn, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

func group(m regexp2.Match, i int) string { return m.GroupByNumber(i).String() }

// FormatMessage converts Markdown to the Google Chat dialect with code protected and invisible codepoints stripped
func FormatMessage(content string) string {
	if content == "" {
		return content
	}
	var keys, values []string
	ph := func(value string) string {
		key := fmt.Sprintf("\x00GC%d\x00", len(keys))
		keys, values = append(keys, key), append(values, value)
		return key
	}
	text := sub(fencedRe, content, func(m regexp2.Match) string { return ph(m.String()) })
	text = sub(inlineRe, text, func(m regexp2.Match) string { return ph(m.String()) })
	text = sub(headerRe, text, func(m regexp2.Match) string { return ph("*" + strings.TrimSpace(group(m, 1)) + "*") })
	text = sub(boldItalRe, text, func(m regexp2.Match) string { return ph("*_" + group(m, 1) + "_*") })
	text = sub(boldRe, text, func(m regexp2.Match) string { return ph("*" + group(m, 1) + "*") })
	text = sub(linkRe, text, func(m regexp2.Match) string { return ph("<" + group(m, 2) + "|" + group(m, 1) + ">") })
	text = sub(invisibleRe, text, func(regexp2.Match) string { return "" })
	text = sub(spacesRe, text, func(regexp2.Match) string { return " " })
	for i, key := range keys {
		text = strings.ReplaceAll(text, key, values[i])
	}
	return text
}

func getOr(d map[string]any, key string, def any) any {
	if v, ok := d[key]; ok {
		return v
	}
	return def
}

func requiredStr(m map[string]any, key, context string) (string, error) {
	value := ""
	if v := m[key]; v != nil {
		value = strings.TrimSpace(anyutil.String(v))
	}
	if value == "" {
		return "", fmt.Errorf("%s.%s is required", context, key)
	}
	return value, nil
}

// Copies each truthy source value as str into the destination in the given order
func copyOpt(dst *jsonutil.Object, src map[string]any, keys ...[2]string) *jsonutil.Object {
	for _, k := range keys {
		if v := src[k[0]]; anyutil.IsTrue(v) {
			dst.Set(k[1], anyutil.String(v))
		}
	}
	return dst
}

func requiredList(m map[string]any, key, msg string) ([]any, error) {
	items := m[key]
	if !anyutil.IsTrue(items) {
		return nil, fmt.Errorf("%s", msg)
	}
	list, ok := items.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%s", msg)
	}
	return list, nil
}

func obj(kv ...any) *jsonutil.Object {
	o := jsonutil.NewObject()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func buttonToChat(button map[string]any) (*jsonutil.Object, error) {
	text, err := requiredStr(button, "text", "button")
	if err != nil {
		return nil, err
	}
	action, err := requiredStr(button, "action", "button")
	if err != nil {
		return nil, err
	}
	rawParams := map[string]any{}
	if p := button["parameters"]; anyutil.IsTrue(p) {
		d, ok := jsonutil.AsMap(p)
		if !ok {
			return nil, fmt.Errorf("button.parameters must be an object")
		}
		rawParams = d
	}
	names := make([]string, 0, len(rawParams))
	for k := range rawParams {
		names = append(names, k)
	}
	sort.Strings(names)
	parameters := make([]any, 0, len(names))
	for _, k := range names {
		parameters = append(parameters, obj("key", k, "value", anyutil.String(rawParams[k])))
	}
	return obj("text", text, "onClick", obj("action", obj("function", action, "parameters", parameters))), nil
}

func textWidget(widget map[string]any) (*jsonutil.Object, error) {
	text, err := requiredStr(widget, "text", "widget")
	if err != nil {
		return nil, err
	}
	return obj("textParagraph", obj("text", FormatMessage(text))), nil
}

var cardHTMLEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// Card text shown as written, escaped for the card HTML subset with line breaks kept
func escapeCardText(text string) string {
	return strings.ReplaceAll(cardHTMLEscaper.Replace(text), "\n", "<br>")
}

func plainTextWidget(widget map[string]any) (*jsonutil.Object, error) {
	text, err := requiredStr(widget, "text", "widget")
	if err != nil {
		return nil, err
	}
	return obj("textParagraph", obj("text", escapeCardText(text))), nil
}

func markdownTextWidget(widget map[string]any) (*jsonutil.Object, error) {
	text, err := requiredStr(widget, "text", "widget")
	if err != nil {
		return nil, err
	}
	return obj("textParagraph", obj("text", text, "textSyntax", "MARKDOWN")), nil
}

func codeBlockWidget(widget map[string]any) (*jsonutil.Object, error) {
	text, err := requiredStr(widget, "text", "widget")
	if err != nil {
		return nil, err
	}
	return obj("textParagraph", obj("text", "<pre><code>"+cardHTMLEscaper.Replace(text)+"</code></pre>")), nil
}

func decoratedTextWidget(widget map[string]any) (*jsonutil.Object, error) {
	text, err := requiredStr(widget, "text", "widget")
	if err != nil {
		return nil, err
	}
	decorated := obj("text", FormatMessage(text), "wrapText", anyutil.IsTrue(getOr(widget, "wrap_text", true)))
	return obj("decoratedText", copyOpt(decorated, widget, [2]string{"top_label", "topLabel"}, [2]string{"bottom_label", "bottomLabel"})), nil
}

func imageWidget(widget map[string]any) (*jsonutil.Object, error) {
	url, err := requiredStr(widget, "image_url", "widget")
	if err != nil {
		return nil, err
	}
	return obj("image", copyOpt(obj("imageUrl", url), widget, [2]string{"alt_text", "altText"})), nil
}

func buttonsWidget(widget map[string]any) (*jsonutil.Object, error) {
	rawButtons, err := requiredList(widget, "buttons", "button widgets require at least one button")
	if err != nil {
		return nil, err
	}
	buttons := make([]any, 0, len(rawButtons))
	for _, b := range rawButtons {
		d, ok := jsonutil.AsMap(b)
		if !ok {
			return nil, fmt.Errorf("card buttons must be objects, got %s", anyutil.Kind(b))
		}
		rendered, err := buttonToChat(d)
		if err != nil {
			return nil, err
		}
		buttons = append(buttons, rendered)
	}
	return obj("buttonList", obj("buttons", buttons)), nil
}

func selectionItem(item any) (*jsonutil.Object, error) {
	d, ok := jsonutil.AsMap(item)
	if !ok {
		return nil, fmt.Errorf("selection items must be objects")
	}
	text, err := requiredStr(d, "text", "selection item")
	if err != nil {
		return nil, err
	}
	value, err := requiredStr(d, "value", "selection item")
	if err != nil {
		return nil, err
	}
	return obj("text", text, "value", value, "selected", anyutil.IsTrue(getOr(d, "selected", false))), nil
}

func strOr(v any, def string) string {
	if anyutil.IsTrue(v) {
		return anyutil.String(v)
	}
	return def
}

func selectionWidget(widget map[string]any) (*jsonutil.Object, error) {
	name, err := requiredStr(widget, "name", "widget")
	if err != nil {
		return nil, err
	}
	rawItems, err := requiredList(widget, "items", "selection widgets require at least one item")
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rawItems))
	for _, it := range rawItems {
		rendered, err := selectionItem(it)
		if err != nil {
			return nil, err
		}
		items = append(items, rendered)
	}
	return obj("selectionInput", obj("name", name, "label", strOr(widget["label"], name),
		"type", strOr(widget["selection_type"], "CHECK_BOX"), "items", items)), nil
}

var widgetRenderers = map[string]func(map[string]any) (*jsonutil.Object, error){
	"text":            textWidget,
	"text_paragraph":  textWidget,
	"plain_text":      plainTextWidget,
	"markdown_text":   markdownTextWidget,
	"code_block":      codeBlockWidget,
	"decorated_text":  decoratedTextWidget,
	"buttons":         buttonsWidget,
	"button_list":     buttonsWidget,
	"selection":       selectionWidget,
	"selection_input": selectionWidget,
	"image":           imageWidget,
	"divider":         func(map[string]any) (*jsonutil.Object, error) { return obj("divider", jsonutil.NewObject()), nil },
}

func widgetToChat(widget any) (*jsonutil.Object, error) {
	d, ok := jsonutil.AsMap(widget)
	if !ok {
		return nil, fmt.Errorf("card widgets must be objects")
	}
	widgetType := strings.TrimSpace(strOr(d["type"], ""))
	renderer, ok := widgetRenderers[widgetType]
	if !ok {
		shown := widgetType
		if shown == "" {
			shown = "<missing>"
		}
		return nil, fmt.Errorf("unsupported widget type: %s", shown)
	}
	return renderer(d)
}

func sectionToChat(section any) (*jsonutil.Object, error) {
	d, ok := jsonutil.AsMap(section)
	if !ok {
		return nil, fmt.Errorf("card sections must be objects")
	}
	widgets, err := requiredList(d, "widgets", "card section widgets must contain at least one widget")
	if err != nil {
		return nil, err
	}
	rendered := make([]any, 0, len(widgets))
	for _, w := range widgets {
		r, err := widgetToChat(w)
		if err != nil {
			return nil, err
		}
		rendered = append(rendered, r)
	}
	return copyOpt(obj("widgets", rendered), d, [2]string{"header", "header"}), nil
}

func headerToChat(header any) (*jsonutil.Object, error) {
	d, ok := jsonutil.AsMap(header)
	if !ok {
		return nil, fmt.Errorf("card.header must be an object")
	}
	title, err := requiredStr(d, "title", "card.header")
	if err != nil {
		return nil, err
	}
	rendered := copyOpt(obj("title", title), d, [2]string{"subtitle", "subtitle"})
	if url := d["image_url"]; anyutil.IsTrue(url) {
		rendered.Set("imageUrl", anyutil.String(url))
		rendered.Set("imageType", strOr(d["image_type"], "SQUARE"))
	}
	return copyOpt(rendered, d, [2]string{"image_alt_text", "imageAltText"}), nil
}

// CardSpecToCardsV2 renders a card spec into the Chat Cards v2 wire shape with its exact key order
func CardSpecToCardsV2(cardSpec any) (*jsonutil.Object, error) {
	d, ok := jsonutil.AsMap(cardSpec)
	if !ok {
		return nil, fmt.Errorf("card must be an object")
	}
	rawSections, err := requiredList(d, "sections", "card.sections must contain at least one section")
	if err != nil {
		return nil, err
	}
	sections := make([]any, 0, len(rawSections))
	for _, s := range rawSections {
		r, err := sectionToChat(s)
		if err != nil {
			return nil, err
		}
		sections = append(sections, r)
	}
	card := obj("sections", sections)
	if header := d["header"]; anyutil.IsTrue(header) {
		h, err := headerToChat(header)
		if err != nil {
			return nil, err
		}
		card.Set("header", h)
	}
	return obj("cardId", strOr(d["card_id"], "icebath-card"), "card", card), nil
}
