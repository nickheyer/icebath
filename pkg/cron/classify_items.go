package cron

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
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
	item  *pyjson.Object
	score *pyjson.Object
}

// Objects of a decoded JSON list, entries of any other type left out
func classifyObjects(list []any) []*pyjson.Object {
	items := make([]*pyjson.Object, 0, len(list))
	for _, entry := range list {
		if obj, ok := entry.(*pyjson.Object); ok {
			items = append(items, obj)
		}
	}
	return items
}

// Candidate items of the input file or stdin and the exit status that ends the run when they cannot be read
func loadClassifyItems(inputFile string, stdin io.Reader, stderr io.Writer) ([]*pyjson.Object, int) {
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
	raw := strings.TrimSpace(pystr.TranslateNewlines(string(data)))
	if raw == "" {
		return nil, classifyExitOK
	}
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		fmt.Fprintf(stderr, "%sinput is not valid JSON: %v\n", classifyErrPrefix, err)
		return nil, classifyExitBadInput
	}
	switch value := parsed.(type) {
	case *pyjson.Object:
		if list, ok := value.GetOr("items", nil).([]any); ok {
			return classifyObjects(list), classifyExitOK
		}
		return []*pyjson.Object{value}, classifyExitOK
	case []any:
		return classifyObjects(value), classifyExitOK
	}
	fmt.Fprintf(stderr, "%sexpected a JSON list or {items: [...]}\n", classifyErrPrefix)
	return nil, classifyExitBadInput
}

// Id a surfaced item is reported under for upstream dedup, its position when it carries none
func classifyItemID(item *pyjson.Object, index int) string {
	for _, key := range classifyIDKeys {
		if value, ok := item.Get(key); ok && pyrepr.Truthy(value) {
			return pyrepr.Str(value)
		}
	}
	return fmt.Sprintf("item-%d", index)
}

// Scoring request for the whole batch, each item as a bounded JSON view of its salient fields
func classifyPrompt(items []*pyjson.Object, criteria string) (string, error) {
	lines := []string{"USER IMPORTANCE CRITERIA:\n" + criteria + "\n", "ITEMS:"}
	for i, item := range items {
		view := pyjson.NewObject()
		for _, key := range classifyViewKeys {
			if value, ok := item.Get(key); ok {
				view.Set(key, value)
			}
		}
		if view.Len() == 0 {
			view = item
		}
		text, err := pyjson.Dumps(view, pyjson.EnsureASCII(false))
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("[%d] %s", i, pystr.Slice(text, 0, classifyItemViewChars)))
	}
	lines = append(lines, "\nReturn the JSON array of scores now (one object per item, same order)."+classifyReplyContract)
	return strings.Join(lines, "\n"), nil
}

// Integer value of a JSON integer or boolean, true counting as one and false as zero
func classifyInt(v any) (*big.Int, bool) {
	if flag, ok := v.(bool); ok {
		if flag {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	}
	return pyrepr.Int(v)
}

// Score objects of the model reply by item position, tolerating a markdown fence and prose around the array
func parseClassifyScores(content string, itemCount int, stderr io.Writer) map[int]*pyjson.Object {
	text := strings.TrimSpace(content)
	if strings.HasPrefix(text, "```") {
		text = strings.Trim(text, "`")
		if _, rest, found := strings.Cut(text, "\n"); found {
			text = rest
		}
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		start, end := strings.Index(text, "["), strings.LastIndex(text, "]")
		if start < 0 || end <= start {
			fmt.Fprintf(stderr, "%sclassifier returned no JSON array\n", classifyErrPrefix)
			return nil
		}
		if parsed, err = pyjson.Loads(text[start : end+1]); err != nil {
			fmt.Fprintf(stderr, "%scould not parse classifier output\n", classifyErrPrefix)
			return nil
		}
	}
	list, ok := parsed.([]any)
	if !ok {
		return nil
	}
	limit := big.NewInt(int64(itemCount))
	scores := map[int]*pyjson.Object{}
	for _, entry := range list {
		obj, isObject := entry.(*pyjson.Object)
		if !isObject {
			continue
		}
		index, isInt := classifyInt(obj.GetOr("index", nil))
		if !isInt || index.Sign() < 0 || index.Cmp(limit) >= 0 {
			continue
		}
		scores[int(index.Int64())] = obj
	}
	return scores
}

// First truthy value among the named fields of an item, nil when none is set
func classifyFirstField(item *pyjson.Object, keys ...string) any {
	for _, key := range keys {
		if value := item.GetOr(key, nil); pyrepr.Truthy(value) {
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
		block := fmt.Sprintf("## [%s/10] %s", pyrepr.Str(entry.score.GetOr("score", nil)), pyrepr.Str(title))
		if link := classifyFirstField(entry.item, "url", "link"); link != nil {
			block += "\n" + pyrepr.Str(link)
		}
		if reason := entry.score.GetOr("reason", ""); pyrepr.Truthy(reason) {
			block += "\n_" + pyrepr.Str(reason) + "_"
		}
		blocks = append(blocks, block)
	}
	return strings.Join(blocks, "\n\n")
}

// Indented JSON list of the surfaced items, each with its id, score, reason and the item as given
func renderClassifiedJSON(surfaced []classifiedItem) (string, error) {
	out := make([]any, 0, len(surfaced))
	for _, entry := range surfaced {
		out = append(out, pyjson.ObjectOf(
			"id", classifyItemID(entry.item, entry.index),
			"score", entry.score.GetOr("score", nil),
			"reason", entry.score.GetOr("reason", ""),
			"item", entry.item,
		))
	}
	return pyjson.Dumps(out, pyjson.EnsureASCII(false), pyjson.Indent(2))
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
		if pyrepr.Truthy(reply) {
			return pyrepr.Str(reply), nil
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
		fmt.Fprintf(stderr, "%sclassifier call failed: %s\n", classifyErrPrefix, pyrepr.ExceptionMessage(err))
		return classifyExitClassifierErr
	}
	scores := parseClassifyScores(content, len(items), stderr)
	threshold := big.NewInt(int64(a.Threshold))
	var surfaced []classifiedItem
	for i, item := range items {
		score, scored := scores[i]
		if !scored {
			continue
		}
		if value, isInt := classifyInt(score.GetOr("score", nil)); isInt && value.Cmp(threshold) >= 0 {
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
