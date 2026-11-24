package cron

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Auxiliary task the urgency scorer runs under, its model and timeout set in auxiliary.monitor
const classifyTaskKey = "monitor"

// Reply shape the score parser reads, stated to the model after the closing instruction of the request
const classifyReplyContract = ` Each object has exactly these keys: "index" (the number in brackets before the item), "score" (an integer from 0 for ignorable to 10 for urgent) and "reason" (one short sentence). Output only the JSON array.`

const (
	classifyMaxTokens     = 1024
	classifyItemViewChars = 1200
	classifyErrPrefix     = "cron classify: "
)

// Exit statuses of icebath cron classify, zero covering both surfaced items and a silent run
const (
	classifyExitOK            = 0
	classifyExitUnreadable    = 1
	classifyExitBadInput      = 2
	classifyExitClassifierErr = 4
)

var (
	// Item fields echoed back as the id of a surfaced item, the first truthy one winning
	classifyIDKeys = []string{"id", "guid", "message_id", "url", "link"}
	// Item fields shown to the scorer in place of the whole object
	classifyViewKeys = []string{"title", "subject", "summary", "text", "body", "from", "sender", "url"}
	utf8BOM          = []byte{0xEF, 0xBB, 0xBF}
)

// Flags of icebath cron classify
type ClassifyArgs struct {
	Criteria  string
	Threshold int
	InputFile string
	Format    string
}

// Candidate item kept for delivery with its position in the input and the score object the model returned for it
type classifiedItem struct {
	index int
	item  *jsonutil.Object
	score *jsonutil.Object
}

// Objects of a decoded JSON list, entries of any other type left out
func classifyObjects(list []any) []*jsonutil.Object {
	items := make([]*jsonutil.Object, 0, len(list))
	for _, entry := range list {
		if obj, ok := entry.(*jsonutil.Object); ok {
			items = append(items, obj)
		}
	}
	return items
}

// Candidate items of the input file or stdin and the exit status that ends the run when they cannot be read
func loadClassifyItems(inputFile string, stdin io.Reader, stderr io.Writer) ([]*jsonutil.Object, int) {
	var data []byte
	var err error
	if inputFile != "" {
		data, err = os.ReadFile(inputFile)
		data = bytes.TrimPrefix(data, utf8BOM)
	} else {
		data, err = io.ReadAll(stdin)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%scannot read the items: %v\n", classifyErrPrefix, err)
		return nil, classifyExitUnreadable
	}
	if !utf8.Valid(data) {
		fmt.Fprintf(stderr, "%sinput is not valid UTF-8\n", classifyErrPrefix)
		return nil, classifyExitUnreadable
	}
	raw := strings.TrimSpace(textutil.NormalizeNewlines(string(data)))
	if raw == "" {
		return nil, classifyExitOK
	}
	parsed, err := jsonutil.Decode(raw)
	if err != nil {
		fmt.Fprintf(stderr, "%sinput is not valid JSON: %v\n", classifyErrPrefix, err)
		return nil, classifyExitBadInput
	}
	switch value := parsed.(type) {
	case *jsonutil.Object:
		if list, ok := value.ValueOr("items", nil).([]any); ok {
			return classifyObjects(list), classifyExitOK
		}
		return []*jsonutil.Object{value}, classifyExitOK
	case []any:
		return classifyObjects(value), classifyExitOK
	}
	fmt.Fprintf(stderr, "%sexpected a JSON list or {items: [...]}\n", classifyErrPrefix)
	return nil, classifyExitBadInput
}

// Id a surfaced item is reported under for upstream dedup, its position when it carries none
func classifyItemID(item *jsonutil.Object, index int) string {
	for _, key := range classifyIDKeys {
		if value, ok := item.Get(key); ok && anyutil.IsTrue(value) {
			return anyutil.String(value)
		}
	}
	return fmt.Sprintf("item-%d", index)
}

// Scoring request for the whole batch, each item as a bounded JSON view of its salient fields
func classifyPrompt(items []*jsonutil.Object, criteria string) (string, error) {
	lines := []string{"USER IMPORTANCE CRITERIA:\n" + criteria + "\n", "ITEMS:"}
	for i, item := range items {
		view := jsonutil.NewObject()
		for _, key := range classifyViewKeys {
			if value, ok := item.Get(key); ok {
				view.Set(key, value)
			}
		}
		if view.Len() == 0 {
			view = item
		}
		text, err := jsonutil.Encode(view)
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("[%d] %s", i, textutil.First(text, classifyItemViewChars)))
	}
	lines = append(lines, "\nReturn the JSON array of scores now (one object per item, same order)."+classifyReplyContract)
	return strings.Join(lines, "\n"), nil
}

// Integer value of a JSON integer or boolean, true counting as one and false as zero
func classifyInt(v any) (int64, bool) {
	if flag, ok := v.(bool); ok {
		if flag {
			return 1, true
		}
		return 0, true
	}
	return anyutil.Int(v)
}

// Score objects of the model reply by item position, tolerating a markdown fence and prose around the array
func parseClassifyScores(content string, itemCount int, stderr io.Writer) map[int]*jsonutil.Object {
	text := strings.TrimSpace(content)
	if strings.HasPrefix(text, "```") {
		text = strings.Trim(text, "`")
		if _, rest, found := strings.Cut(text, "\n"); found {
			text = rest
		}
	}
	parsed, err := jsonutil.Decode(text)
	if err != nil {
		start, end := strings.Index(text, "["), strings.LastIndex(text, "]")
		if start < 0 || end <= start {
			fmt.Fprintf(stderr, "%sclassifier returned no JSON array\n", classifyErrPrefix)
			return nil
		}
		if parsed, err = jsonutil.Decode(text[start : end+1]); err != nil {
			fmt.Fprintf(stderr, "%scould not parse classifier output\n", classifyErrPrefix)
			return nil
		}
	}
	list, ok := parsed.([]any)
	if !ok {
		return nil
	}
	limit := int64(itemCount)
	scores := map[int]*jsonutil.Object{}
	for _, entry := range list {
		obj, isObject := entry.(*jsonutil.Object)
		if !isObject {
			continue
		}
		index, isInt := classifyInt(obj.ValueOr("index", nil))
		if !isInt || index < 0 || index >= limit {
			continue
		}
		scores[int(index)] = obj
	}
	return scores
}

// First truthy value among the named fields of an item, nil when none is set
func classifyFirstField(item *jsonutil.Object, keys ...string) any {
	for _, key := range keys {
		if value := item.ValueOr(key, nil); anyutil.IsTrue(value) {
			return value
		}
	}
	return nil
}

// Markdown blocks of the surfaced items, a scored heading then the link and the reason when present
func renderClassifiedText(surfaced []classifiedItem) string {
	blocks := make([]string, 0, len(surfaced))
	for _, entry := range surfaced {
		title := classifyFirstField(entry.item, "title", "subject", "summary")
		if title == nil {
			title = classifyItemID(entry.item, entry.index)
		}
		block := fmt.Sprintf("## [%s/10] %s", anyutil.String(entry.score.ValueOr("score", nil)), anyutil.String(title))
		if link := classifyFirstField(entry.item, "url", "link"); link != nil {
			block += "\n" + anyutil.String(link)
		}
		if reason := entry.score.ValueOr("reason", ""); anyutil.IsTrue(reason) {
			block += "\n_" + anyutil.String(reason) + "_"
		}
		blocks = append(blocks, block)
	}
	return strings.Join(blocks, "\n\n")
}

// Indented JSON list of the surfaced items, each with its id, score, reason and the item as given
func renderClassifiedJSON(surfaced []classifiedItem) (string, error) {
	out := make([]any, 0, len(surfaced))
	for _, entry := range surfaced {
		out = append(out, jsonutil.ObjectOf(
			"id", classifyItemID(entry.item, entry.index),
			"score", entry.score.ValueOr("score", nil),
			"reason", entry.score.ValueOr("reason", ""),
			"item", entry.item,
		))
	}
	return jsonutil.Encode(out, jsonutil.Indent(2))
}

// Reply text of one scoring call to the auxiliary monitor model, a panic of the call reported as its error
func classifyCall(ctx context.Context, prompt string) (content string, err error) {
	defer func() {
		if failure := recover(); failure != nil {
			content, err = "", fmt.Errorf("%v", failure)
		}
	}()
	response, err := provider.CallLLM(ctx, map[string]any{
		"task":        classifyTaskKey,
		"messages":    []any{map[string]any{"role": "user", "content": prompt}},
		"max_tokens":  classifyMaxTokens,
		"temperature": 0,
	})
	if err != nil {
		return "", err
	}
	fields, _ := response.(map[string]any)
	choices, _ := fields["choices"].([]any)
	if len(choices) == 0 {
		return "", fmt.Errorf("the reply carries no choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	switch reply := message["content"].(type) {
	case string:
		return reply, nil
	default:
		if anyutil.IsTrue(reply) {
			return anyutil.String(reply), nil
		}
		return "", nil
	}
}

// ClassifyItemsCommand scores the candidate items on stdin or in the input file in one auxiliary monitor call and prints only those at or above the threshold, returning the exit status: empty stdout is the silence a cron job delivers nothing for, and a failed scoring call is non-zero so the job reports it
func ClassifyItemsCommand(ctx context.Context, a ClassifyArgs, stdin io.Reader, stdout, stderr io.Writer) int {
	items, code := loadClassifyItems(a.InputFile, stdin, stderr)
	if code != classifyExitOK || len(items) == 0 {
		return code
	}
	prompt, err := classifyPrompt(items, a.Criteria)
	if err != nil {
		fmt.Fprintf(stderr, "%scannot render the items: %v\n", classifyErrPrefix, err)
		return classifyExitUnreadable
	}
	content, err := classifyCall(ctx, prompt)
	if err != nil {
		fmt.Fprintf(stderr, "%sclassifier call failed: %s\n", classifyErrPrefix, err.Error())
		return classifyExitClassifierErr
	}
	scores := parseClassifyScores(content, len(items), stderr)
	threshold := int64(a.Threshold)
	var surfaced []classifiedItem
	for i, item := range items {
		score, scored := scores[i]
		if !scored {
			continue
		}
		if value, isInt := classifyInt(score.ValueOr("score", nil)); isInt && value >= threshold {
			surfaced = append(surfaced, classifiedItem{index: i, item: item, score: score})
		}
	}
	if len(surfaced) == 0 {
		return classifyExitOK
	}
	text := renderClassifiedText(surfaced)
	if a.Format == "json" {
		if text, err = renderClassifiedJSON(surfaced); err != nil {
			fmt.Fprintf(stderr, "%scannot render the surfaced items: %v\n", classifyErrPrefix, err)
			return classifyExitUnreadable
		}
	}
	fmt.Fprintln(stdout, text)
	return classifyExitOK
}
