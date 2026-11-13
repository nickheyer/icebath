package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	reactionURI           = "/open-apis/drive/v2/files/:file_token/comments/reaction"
	batchQueryMetaURI     = "/open-apis/drive/v1/metas/batch_query"
	batchQueryCommentURI  = "/open-apis/drive/v1/files/:file_token/comments/batch_query"
	listCommentsURI       = "/open-apis/drive/v1/files/:file_token/comments"
	repliesURI            = "/open-apis/drive/v1/files/:file_token/comments/:comment_id/replies"
	addCommentURI         = "/open-apis/drive/v1/files/:file_token/new_comments"
	wikiGetNodeURI        = "/open-apis/wiki/v2/spaces/get_node"
	commentRetryLimit     = 6
	commentMaxPages       = 5
	replyChunkSize        = 4000
	promptTextLimit       = 220
	localTimelineLimit    = 20
	wholeTimelineLimit    = 12
	mentionNote           = "This comment mentioned you (@mention is for routing, not task content)."
	noReplySentinel       = "NO_REPLY"
	sessionMaxMessages    = 50
	sessionTTLSeconds     = 3600
	commentRetryDelayTime = time.Second
)

var reactionVerbs = map[string]string{"add": "added", "delete": "deleted"}
var elementTextKeys = map[string]string{"text_run": "text", "docs_link": "url"}
var allowedNoticeTypes = map[string]bool{"add_comment": true, "add_reply": true}

var feishuDocURLRe = pyre.MustCompile(`(?:feishu\.cn|larkoffice\.com|larksuite\.com|lark\.suite\.com)` +
	`/(?P<doc_type>wiki|doc|docx|sheet|sheets|slides|mindnote|bitable|base|file)` + `/(?P<token>[A-Za-z0-9_-]{10,40})`)

const commonInstructions = `This is a Feishu document comment thread, not an IM chat.
Do NOT call feishu_drive_add_comment or feishu_drive_reply_comment yourself.
Your reply will be posted automatically. Just output the reply text.
Use the thread timeline above as the main context.
If the quoted content is not enough, use feishu_doc_read to read nearby context.
The quoted content is your primary anchor — insert/summarize/explain requests are about it.
Do not guess document content you haven't read.
Reply in the same language as the user's comment unless they request otherwise.
Use plain text only. Do not use Markdown, headings, bullet lists, tables, or code blocks.
Do not show your reasoning process. Do not start with "I will", "Let me", or "I'll first".
Output only the final user-facing reply.
If no reply is needed, output exactly NO_REPLY.`

// Attribute access on a value that is not a mapping, the AttributeError the Python handler would raise
type attrError struct{ msg string }

func (e attrError) Error() string { return e.msg }

// Mapping view of a decoded value, raising the attribute error a dict get on anything else raises
func mustObj(v any) *pyjson.Object {
	if o, ok := v.(*pyjson.Object); ok {
		return o
	}
	panic(attrError{fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))})
}

// Mapping of a dict value, empty for anything else
func asDictObj(v any) *pyjson.Object {
	if o, ok := v.(*pyjson.Object); ok {
		return o
	}
	return pyjson.NewObject()
}

type badJSON struct{}

// Decoded JSON of a string value, fallback when it does not decode, other values passed through
func maybeJSON(value any, fallback any) any {
	s, ok := value.(string)
	if !ok {
		return value
	}
	v, err := pyjson.Loads(s)
	if err != nil {
		return fallback
	}
	return v
}

func reprPairs(pairs [][2]string) string {
	if pairs == nil {
		return "None"
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = "(" + pyrepr.StrRepr(p[0]) + ", " + pyrepr.StrRepr(p[1]) + ")"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func reprPaths(paths [][2]string) string {
	if paths == nil {
		return "None"
	}
	parts := make([]string, len(paths))
	for i, p := range paths {
		parts[i] = pyrepr.StrRepr(p[0]) + ": " + pyrepr.StrRepr(p[1])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Executes a tenant token API request and returns its code, message and data object
func execRequest(ctx context.Context, client larkAPI, method, uri string, paths, queries [][2]string, body any) (any, string, *pyjson.Object) {
	bodyText := "None"
	if pyrepr.Truthy(body) {
		bodyText = pystr.Slice(dumpsNoASCII(body), 0, 500)
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] API >>> %s %s paths=%s queries=%s body=%s", method, uri, reprPaths(paths), reprPairs(queries), bodyText))
	httpMethod := http.MethodPost
	if method == "GET" {
		httpMethod = http.MethodGet
	}
	req := &larkcore.ApiReq{HttpMethod: httpMethod, ApiPath: uri, PathParams: larkcore.PathParams{}, QueryParams: larkcore.QueryParams{},
		SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant}}
	for _, p := range paths {
		req.PathParams.Set(p[0], p[1])
	}
	for _, q := range queries {
		req.QueryParams.Add(q[0], q[1])
	}
	if body != nil {
		req.Body = body
	}
	resp, err := client.Do(ctx, req)
	if err != nil {
		panic(attrError{err.Error()})
	}
	var code any
	msg := ""
	data := pyjson.NewObject()
	hasRaw := resp != nil && resp.RawBody != nil
	if hasRaw {
		if parsed, perr := pyjson.Loads(string(resp.RawBody)); perr == nil {
			if root, ok := parsed.(*pyjson.Object); ok {
				code = dget(root, "code")
				msg = pyrepr.Str(dgetDef(root, "msg", ""))
				if d, ok := dgetDef(root, "data", pyjson.NewObject()).(*pyjson.Object); ok {
					data = d
				}
			}
		}
	}
	keys := "empty"
	if data.Len() > 0 {
		keys = pyrepr.Repr(anyStrings(data.Keys))
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] API <<< %s %s code=%s msg=%s data_keys=%s", method, uri, pyrepr.Str(code), msg, keys))
	if !numberIs(code, 0) {
		shown := ""
		if hasRaw {
			shown = pystr.Slice(string(resp.RawBody), 0, 500)
		}
		slog.Warn("[Feishu-Comment] API FAIL raw response: " + shown)
	}
	return code, msg, data
}

func anyStrings(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func codeOK(code any) bool { return numberIs(code, 0) }

// Warning on a failed code, info otherwise
func logOutcome(code any, fail, ok string) {
	if !codeOK(code) {
		slog.Warn(fail)
	} else {
		slog.Info(ok)
	}
}

// Flat field dict of a drive comment event, nil when it carries no event
func parseDriveCommentEvent(data any) (map[string]string, bool, bool) {
	slog.Debug("[Feishu-Comment] parse_drive_comment_event: data type=" + pyrepr.TypeName(data))
	root := asDictObj(maybeJSON(data, pyjson.NewObject()))
	event, ok := root.Get("event")
	if !ok || event == nil {
		slog.Debug("[Feishu-Comment] parse_drive_comment_event: no .event attribute, returning None")
		return nil, false, false
	}
	evt := asDictObj(event)
	slog.Debug("[Feishu-Comment] parse_drive_comment_event: evt keys=" + pyrepr.Repr(anyStrings(evt.Keys)))
	meta := asDictObj(orObj(dget(evt, "notice_meta")))
	fields := map[string]string{}
	for _, k := range []string{"event_id", "comment_id", "reply_id", "timestamp"} {
		fields[k] = strOr(dget(evt, k))
	}
	for _, k := range []string{"file_token", "file_type", "notice_type"} {
		fields[k] = strOr(dget(meta, k))
	}
	fields["from_open_id"] = strOr(dget(asDictObj(orObj(dget(meta, "from_user_id"))), "open_id"))
	fields["to_open_id"] = strOr(dget(asDictObj(orObj(dget(meta, "to_user_id"))), "open_id"))
	return fields, pyrepr.Truthy(dget(evt, "is_mentioned")), true
}

func orObj(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return pyjson.NewObject()
}

func obj(pairs ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

// Adds or deletes an emoji reaction on a comment reply, reporting success
func updateCommentReaction(ctx context.Context, client larkAPI, action, fileToken, fileType, replyID, reactionType string) bool {
	code, msg, _ := execRequest(ctx, client, "POST", reactionURI, [][2]string{{"file_token", fileToken}}, [][2]string{{"file_type", fileType}},
		obj("action", action, "reply_id", replyID, "reaction_type", reactionType))
	logOutcome(code, fmt.Sprintf("[Feishu-Comment] Reaction API failed: code=%s msg=%s file=%s:%s reply=%s", pyrepr.Str(code), msg, fileType, fileToken, replyID),
		fmt.Sprintf("[Feishu-Comment] Reaction '%s' %s: file=%s:%s reply=%s", reactionType, reactionVerbs[action], fileType, fileToken, replyID))
	return codeOK(code)
}

// Title, URL and doc type of a document from the batch meta API, empty on failure
func queryDocumentMeta(ctx context.Context, client larkAPI, fileToken, fileType string) *pyjson.Object {
	slog.Debug(fmt.Sprintf("[Feishu-Comment] query_document_meta: file_token=%s file_type=%s", fileToken, fileType))
	code, msg, data := execRequest(ctx, client, "POST", batchQueryMetaURI, nil, nil,
		obj("request_docs", []any{obj("doc_token", fileToken, "doc_type", fileType)}, "with_url", true))
	if !codeOK(code) {
		slog.Warn(fmt.Sprintf("[Feishu-Comment] Meta batch_query failed: code=%s msg=%s", pyrepr.Str(code), msg))
		return pyjson.NewObject()
	}
	metas := dgetDef(data, "metas", []any{})
	slog.Debug(fmt.Sprintf("[Feishu-Comment] query_document_meta: raw metas type=%s value=%s", pyrepr.TypeName(metas), pystr.Slice(pyrepr.Str(metas), 0, 300)))
	_, isDict := metas.(*pyjson.Object)
	if !pyrepr.Truthy(metas) && !isDict {
		slog.Debug("[Feishu-Comment] query_document_meta: no metas found")
		return pyjson.NewObject()
	}
	var meta *pyjson.Object
	switch m := metas.(type) {
	case []any:
		meta = mustObj(m[0])
	case *pyjson.Object:
		if m.Len() > 0 {
			meta = pyjson.NewObject()
		} else {
			meta = mustObj(dgetDef(m, fileToken, pyjson.NewObject()))
		}
	default:
		meta = pyjson.NewObject()
	}
	result := obj("title", dgetDef(meta, "title", ""), "url", dgetDef(meta, "url", ""), "doc_type", dgetDef(meta, "doc_type", fileType))
	url := pyrepr.Str(dget(result, "url"))
	if !pyrepr.Truthy(dget(result, "url")) {
		url = ""
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] query_document_meta: title=%s url=%s", pyrepr.Str(dget(result, "title")), pystr.Slice(url, 0, 80)))
	return result
}

// Logs and sleeps between retries, logging the final failure on the last attempt
func retryPause(ctx context.Context, attempt int, retryMsg, failMsg string) bool {
	if attempt < commentRetryLimit-1 {
		slog.Info(retryMsg)
		return sleepCtx(ctx, commentRetryDelayTime)
	}
	slog.Warn(failMsg)
	return false
}

// One comment's details with retries while it is not queryable yet, empty on failure
func batchQueryComment(ctx context.Context, client larkAPI, fileToken, fileType, commentID string) *pyjson.Object {
	slog.Debug(fmt.Sprintf("[Feishu-Comment] batch_query_comment: file_token=%s comment_id=%s", fileToken, commentID))
	var data *pyjson.Object
	for attempt := 0; attempt < commentRetryLimit; attempt++ {
		code, msg, d := execRequest(ctx, client, "POST", batchQueryCommentURI, [][2]string{{"file_token", fileToken}},
			[][2]string{{"file_type", fileType}, {"user_id_type", "open_id"}}, obj("comment_ids", []any{commentID}))
		data = d
		if codeOK(code) {
			break
		}
		if !retryPause(ctx, attempt, fmt.Sprintf("[Feishu-Comment] batch_query_comment retry %d/%d: code=%s msg=%s", attempt+1, commentRetryLimit, pyrepr.Str(code), msg),
			fmt.Sprintf("[Feishu-Comment] batch_query_comment failed after %d attempts: code=%s msg=%s", commentRetryLimit, pyrepr.Str(code), msg)) {
			return pyjson.NewObject()
		}
	}
	items := dgetDef(data, "items", []any{})
	list, isList := items.([]any)
	n := 0
	if isList {
		n = len(list)
	}
	slog.Debug(fmt.Sprintf("[Feishu-Comment] batch_query_comment: got %d items", n))
	if !pyrepr.Truthy(items) || !isList {
		slog.Warn("[Feishu-Comment] batch_query_comment: empty items, raw data keys=" + pyrepr.Repr(anyStrings(data.Keys)))
		return pyjson.NewObject()
	}
	item := mustObj(list[0])
	replyCount := "?"
	if rl, ok := dget(item, "reply_list").(*pyjson.Object); ok {
		replies, _ := dgetDef(rl, "replies", []any{}).([]any)
		replyCount = fmt.Sprint(len(replies))
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] batch_query_comment: is_whole=%s quote=%s reply_count=%s", pyrepr.Str(dget(item, "is_whole")),
		pystr.Slice(strOr(dgetDef(item, "quote", "")), 0, 60), replyCount))
	return item
}

// Up to five pages of items of a listing endpoint and whether every page fetched
func listAllPages(ctx context.Context, client larkAPI, uri string, paths, queries [][2]string, failFmt, pageFmt string) ([]any, bool) {
	var out []any
	pageToken := ""
	for i := 0; i < commentMaxPages; i++ {
		q := append([][2]string{}, queries...)
		if pageToken != "" {
			q = append(q, [2]string{"page_token", pageToken})
		}
		code, msg, data := execRequest(ctx, client, "GET", uri, paths, q, nil)
		if !codeOK(code) {
			slog.Warn(fmt.Sprintf(failFmt, pyrepr.Str(code), msg))
			return out, false
		}
		items, isList := dgetDef(data, "items", []any{}).([]any)
		if isList {
			out = append(out, items...)
			if pageFmt != "" {
				slog.Debug(fmt.Sprintf(pageFmt, len(items), len(out)))
			}
		}
		pageToken = ""
		if pyrepr.Truthy(dget(data, "has_more")) {
			pageToken = strOrRaw(dgetDef(data, "page_token", ""))
		}
		if pageToken == "" {
			break
		}
	}
	return out, true
}

func strOrRaw(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Every whole document comment, paginated
func listWholeComments(ctx context.Context, client larkAPI, fileToken, fileType string) []any {
	slog.Debug("[Feishu-Comment] list_whole_comments: file_token=" + fileToken)
	all, _ := listAllPages(ctx, client, listCommentsURI, [][2]string{{"file_token", fileToken}},
		[][2]string{{"file_type", fileType}, {"is_whole", "true"}, {"page_size", "100"}, {"user_id_type", "open_id"}},
		"[Feishu-Comment] List whole comments failed: code=%s msg=%s", "[Feishu-Comment] list_whole_comments: page got %d items, total=%d")
	slog.Info(fmt.Sprintf("[Feishu-Comment] list_whole_comments: total %d whole comments fetched", len(all)))
	return all
}

// Every reply of a thread, retrying while an expected reply id is not listed yet
func listCommentReplies(ctx context.Context, client larkAPI, fileToken, fileType, commentID, expectReplyID string) []any {
	slog.Debug(fmt.Sprintf("[Feishu-Comment] list_comment_replies: file_token=%s comment_id=%s", fileToken, commentID))
	var all []any
	for attempt := 0; attempt < commentRetryLimit; attempt++ {
		var ok bool
		all, ok = listAllPages(ctx, client, repliesURI, [][2]string{{"file_token", fileToken}, {"comment_id", commentID}},
			[][2]string{{"file_type", fileType}, {"page_size", "100"}, {"user_id_type", "open_id"}}, "[Feishu-Comment] List replies failed: code=%s msg=%s", "")
		found := false
		for _, r := range all {
			if pyrepr.Equal(dget(mustObj(r), "reply_id"), expectReplyID) {
				found = true
				break
			}
		}
		if expectReplyID == "" || !ok || found {
			break
		}
		retryPause(ctx, attempt, fmt.Sprintf("[Feishu-Comment] list_comment_replies: reply_id=%s not found, retry %d/%d", expectReplyID, attempt+1, commentRetryLimit),
			fmt.Sprintf("[Feishu-Comment] list_comment_replies: reply_id=%s not found after %d attempts", expectReplyID, commentRetryLimit))
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] list_comment_replies: total %d replies fetched", len(all)))
	return all
}

// Escapes the characters text_run content does not allow
func sanitizeCommentText(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// Posts a reply to a local comment thread, returning success and the code
func replyToComment(ctx context.Context, client larkAPI, fileToken, fileType, commentID, text string) (bool, any) {
	text = sanitizeCommentText(text)
	slog.Info(fmt.Sprintf("[Feishu-Comment] reply_to_comment: comment_id=%s text=%s", commentID, pystr.Slice(text, 0, 100)))
	code, msg, _ := execRequest(ctx, client, "POST", repliesURI, [][2]string{{"file_token", fileToken}, {"comment_id", commentID}},
		[][2]string{{"file_type", fileType}}, obj("content", obj("elements", []any{obj("type", "text_run", "text_run", obj("text", text))})))
	logOutcome(code, fmt.Sprintf("[Feishu-Comment] reply_to_comment FAILED: code=%s msg=%s comment_id=%s", pyrepr.Str(code), msg, commentID),
		"[Feishu-Comment] reply_to_comment OK: comment_id="+commentID)
	return codeOK(code), code
}

// Adds a new whole document comment, reporting success
func addWholeComment(ctx context.Context, client larkAPI, fileToken, fileType, text string) bool {
	text = sanitizeCommentText(text)
	slog.Info(fmt.Sprintf("[Feishu-Comment] add_whole_comment: file_token=%s text=%s", fileToken, pystr.Slice(text, 0, 100)))
	code, msg, _ := execRequest(ctx, client, "POST", addCommentURI, [][2]string{{"file_token", fileToken}}, nil,
		obj("file_type", fileType, "reply_elements", []any{obj("type", "text", "text", text)}))
	logOutcome(code, fmt.Sprintf("[Feishu-Comment] add_whole_comment FAILED: code=%s msg=%s", pyrepr.Str(code), msg), "[Feishu-Comment] add_whole_comment OK")
	return codeOK(code)
}

// Delivery chunks of text, cut at the last line break inside the limit when there is one
func chunkText(text string, limit int) []string {
	var chunks []string
	r := []rune(text)
	for len(r) > limit {
		cut := limit
		if nl := lastIndexRune(r[:limit], '\n'); nl > 0 {
			cut = nl
		}
		chunks = append(chunks, string(r[:cut]))
		r = []rune(strings.TrimLeft(string(r[cut:]), "\n"))
	}
	if len(r) > 0 || len(chunks) == 0 {
		chunks = append(chunks, string(r))
	}
	return chunks
}

func lastIndexRune(r []rune, c rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == c {
			return i
		}
	}
	return -1
}

// Posts the agent reply in chunks, a local reply falling back to whole comments when replies are not allowed
func deliverCommentReply(ctx context.Context, client larkAPI, fileToken, fileType, commentID, text string, isWhole bool) bool {
	chunks := chunkText(text, replyChunkSize)
	slog.Info(fmt.Sprintf("[Feishu-Comment] deliver_comment_reply: is_whole=%s comment_id=%s text_len=%d chunks=%d", pyBool(isWhole), commentID,
		pystr.Len(text), len(chunks)))
	for i, chunk := range chunks {
		if len(chunks) > 1 {
			slog.Info(fmt.Sprintf("[Feishu-Comment] deliver_comment_reply: sending chunk %d/%d (%d chars)", i+1, len(chunks), pystr.Len(chunk)))
		}
		ok, code := true, any(int64(0))
		if !isWhole {
			ok, code = replyToComment(ctx, client, fileToken, fileType, commentID, chunk)
		}
		if !isWhole && !ok && numberIs(code, 1069302) {
			slog.Info("[Feishu-Comment] Reply not allowed (1069302), falling back to add_whole_comment")
			isWhole = true
		}
		if isWhole {
			ok = addWholeComment(ctx, client, fileToken, fileType, chunk)
		}
		if !ok {
			return false
		}
	}
	return true
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// Plain text of a reply's elements, the semantic form dropping the self mention and collapsing whitespace
func extractReplyText(reply *pyjson.Object, semantic bool, selfOpenID string) string {
	raw := dgetDef(reply, "content", pyjson.NewObject())
	content := maybeJSON(raw, badJSON{})
	if _, bad := content.(badJSON); bad {
		return pyrepr.Str(raw)
	}
	var parts []string
	elements, _ := dgetDef(mustObj(content), "elements", []any{}).([]any)
	for _, e := range elements {
		elem := mustObj(e)
		etype := pyrepr.Str(dget(elem, "type"))
		var uid any
		if etype == "person" {
			def := any("unknown")
			if semantic {
				def = ""
			}
			uid = dgetDef(mustObj(dgetDef(elem, "person", pyjson.NewObject())), "user_id", def)
		}
		if key, ok := elementTextKeys[etype]; ok {
			parts = append(parts, pyrepr.Str(dgetDef(mustObj(dgetDef(elem, etype, pyjson.NewObject())), key, "")))
		} else if etype == "person" && !(semantic && selfOpenID != "" && pyrepr.Equal(uid, selfOpenID)) {
			parts = append(parts, "@"+pyrepr.Str(uid))
		}
	}
	text := strings.Join(parts, "")
	if semantic {
		return pystr.Strip(strings.Join(pystr.SplitWhitespace(text, -1), " "))
	}
	return text
}

// User id of a reply from its open id or user id
func getReplyUserID(reply *pyjson.Object) string {
	userID := dgetDef(reply, "user_id", "")
	if u, ok := userID.(*pyjson.Object); ok {
		return firstNonEmpty(strOr(dgetDef(u, "open_id", "")), strOr(dgetDef(u, "user_id", "")))
	}
	return pyrepr.Str(userID)
}

// Replies of a whole comment, its reply list possibly a JSON string
func replyListReplies(whole *pyjson.Object) []any {
	list := mustObj(maybeJSON(dgetDef(whole, "reply_list", pyjson.NewObject()), pyjson.NewObject()))
	out, _ := dgetDef(list, "replies", []any{}).([]any)
	return out
}

type docLink struct {
	URL, DocType, Token          string
	ResolvedType, ResolvedToken  string
	resolvedTypeSet, resolvedSet bool
}

// Unique Feishu document links found in reply elements
func extractDocsLinks(replies []any) []*docLink {
	seen := map[string]bool{}
	var links []*docLink
	for _, r := range replies {
		content := maybeJSON(dgetDef(mustObj(r), "content", pyjson.NewObject()), badJSON{})
		if _, bad := content.(badJSON); bad {
			continue
		}
		elements, _ := dgetDef(mustObj(content), "elements", []any{}).([]any)
		for _, e := range elements {
			elem := mustObj(e)
			url := ""
			if t := pyrepr.Str(dget(elem, "type")); t == "docs_link" || t == "link" {
				src := dget(elem, "docs_link")
				if !pyrepr.Truthy(src) {
					src = dget(elem, "link")
				}
				url = pyrepr.Str(dgetDef(mustObj(orObj(src)), "url", ""))
			}
			if url == "" {
				continue
			}
			m, err := feishuDocURLRe.FindStringMatch(url)
			if err != nil || m == nil {
				continue
			}
			token := m.GroupByName("token").String()
			if seen[token] {
				continue
			}
			seen[token] = true
			links = append(links, &docLink{URL: url, DocType: m.GroupByName("doc_type").String(), Token: token})
		}
	}
	return links
}

// Wiki node of the queries, nil after logging an API failure
func wikiNode(ctx context.Context, client larkAPI, queries [][2]string, failMsg func(code any, msg string) string) *pyjson.Object {
	code, msg, data := execRequest(ctx, client, "GET", wikiGetNodeURI, nil, queries, nil)
	if !codeOK(code) {
		slog.Warn(failMsg(code, msg))
		return nil
	}
	return mustObj(dgetDef(data, "node", pyjson.NewObject()))
}

// Wiki node token owning a document token, empty when it is not a wiki document
func reverseLookupWikiToken(ctx context.Context, client larkAPI, objType, objToken string) string {
	node := wikiNode(ctx, client, [][2]string{{"token", objToken}, {"obj_type", objType}}, func(code any, msg string) string {
		return fmt.Sprintf("[Feishu-Comment] Wiki reverse lookup failed: code=%s msg=%s obj=%s:%s", pyrepr.Str(code), msg, objType, objToken)
	})
	if node == nil {
		return ""
	}
	return strOrRaw(dgetDef(node, "node_token", ""))
}

// Annotates wiki links with the document type and token they resolve to
func resolveWikiNodes(ctx context.Context, client larkAPI, links []*docLink) []*docLink {
	for _, link := range links {
		if link.DocType != "wiki" {
			continue
		}
		token := link.Token
		node := wikiNode(ctx, client, [][2]string{{"token", token}}, func(code any, msg string) string {
			return fmt.Sprintf("[Feishu-Comment] Wiki resolve failed: code=%s msg=%s token=%s", pyrepr.Str(code), msg, token)
		})
		if node == nil {
			continue
		}
		rtype, rtoken := strOrRaw(dgetDef(node, "obj_type", "")), strOrRaw(dgetDef(node, "obj_token", ""))
		if rtype != "" && rtoken != "" {
			slog.Info(fmt.Sprintf("[Feishu-Comment] Wiki resolved: %s -> %s:%s", token, rtype, rtoken))
			link.ResolvedType, link.ResolvedToken, link.resolvedTypeSet, link.resolvedSet = rtype, rtoken, true, true
		} else {
			slog.Warn("[Feishu-Comment] Wiki resolve returned empty: " + token)
		}
	}
	return links
}

// Referenced documents block for the prompt, empty without links
func formatReferencedDocs(links []*docLink, currentFileToken string) string {
	if len(links) == 0 {
		return ""
	}
	lines := []string{"", "Referenced documents in comments:"}
	for _, l := range links {
		rtype, rtoken := l.DocType, l.Token
		if l.resolvedTypeSet {
			rtype = l.ResolvedType
		}
		if l.resolvedSet {
			rtoken = l.ResolvedToken
		}
		same := ""
		if rtoken == currentFileToken {
			same = " (same as current document)"
		}
		lines = append(lines, fmt.Sprintf("- %s:%s%s (%s)", rtype, rtoken, same, pystr.Slice(l.URL, 0, 80)))
	}
	return strings.Join(lines, "\n")
}

func referencedDocsText(ctx context.Context, client larkAPI, replies []any, fileToken string) string {
	links := extractDocsLinks(replies)
	if len(links) > 0 {
		links = resolveWikiNodes(ctx, client, links)
	}
	return formatReferencedDocs(links, fileToken)
}

// Text cut to limit code points with an ellipsis
func truncateText(text string, limit int) string {
	if pystr.Len(text) <= limit {
		return text
	}
	return pystr.Slice(text, 0, limit) + "..."
}

type timelineEntry struct {
	UserID string
	Text   string
	IsSelf bool
}

// Up to limit entries around center plus the pinned ones, the last limit entries when nothing is selectable
func selectTimeline(timeline []timelineEntry, limit, center int, pinned []int) []timelineEntry {
	n := len(timeline)
	if n <= limit {
		return timeline
	}
	selected := map[int]bool{}
	for _, i := range append(append([]int{}, pinned...), center) {
		if i >= 0 && i < n {
			selected[i] = true
		}
	}
	lo, hi := center-1, center+1
	for len(selected) < limit && (lo >= 0 || hi < n) {
		if lo >= 0 {
			selected[lo] = true
		}
		if len(selected) < limit && hi < n {
			selected[hi] = true
		}
		lo, hi = lo-1, hi+1
	}
	if len(selected) == 0 {
		return timeline[n-limit:]
	}
	idx := make([]int, 0, len(selected))
	for i := range selected {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]timelineEntry, len(idx))
	for j, i := range idx {
		out[j] = timeline[i]
	}
	return out
}

// Intro, document block, selected timeline, referenced documents and the common instructions
func buildCommentPrompt(intro []string, docURL, fileType, fileToken string, ids []string, label string, timeline, selected []timelineEntry, referencedDocs string) string {
	lines := append(append([]string{}, intro...), mentionNote, "Document link: "+docURL, "Current commented document:", "- file_type="+fileType, "- file_token="+fileToken)
	lines = append(lines, ids...)
	lines = append(lines, "", fmt.Sprintf("%s (%d/%d entries):", label, len(selected), len(timeline)))
	for _, e := range selected {
		you := ""
		if e.IsSelf {
			you = " <-- YOU"
		}
		lines = append(lines, "["+e.UserID+"] "+truncateText(e.Text, promptTextLimit)+you)
	}
	if referencedDocs != "" {
		lines = append(lines, referencedDocs)
	}
	return strings.Join(append(lines, "", commonInstructions), "\n")
}

// Prompt fields shared by both comment prompt kinds
type commentDoc struct {
	Title, URL, FileToken, FileType, SelfOpenID string
}

// Prompt for a reply on a local quoted text comment
func buildLocalCommentPrompt(doc commentDoc, commentID, quoteText, rootText, targetText string, timeline []timelineEntry, targetIndex int, referencedDocs string) string {
	intro := []string{fmt.Sprintf(`The user added a reply in "%s".`, doc.Title), fmt.Sprintf(`Current user comment text: "%s"`, truncateText(targetText, promptTextLimit)),
		fmt.Sprintf(`Original comment text: "%s"`, truncateText(rootText, promptTextLimit)), fmt.Sprintf(`Quoted content: "%s"`, truncateText(quoteText, 500))}
	selected := selectTimeline(timeline, localTimelineLimit, targetIndex, []int{0, len(timeline) - 1})
	return buildCommentPrompt(intro, doc.URL, doc.FileType, doc.FileToken, []string{"- comment_id=" + commentID}, "Current comment card timeline", timeline, selected, referencedDocs)
}

// Prompt for a whole document comment
func buildWholeCommentPrompt(doc commentDoc, commentText string, timeline []timelineEntry, currentIndex, nearestSelfIndex int, referencedDocs string) string {
	intro := []string{fmt.Sprintf(`The user added a comment in "%s".`, doc.Title), fmt.Sprintf(`Current user comment text: "%s"`, truncateText(commentText, promptTextLimit)),
		"This is a whole-document comment."}
	selected := selectTimeline(timeline, wholeTimelineLimit, currentIndex, []int{nearestSelfIndex})
	return buildCommentPrompt(intro, doc.URL, doc.FileType, doc.FileToken, nil, "Whole-document comment timeline", timeline, selected, referencedDocs)
}

func commentSessionKey(fileType, fileToken string) string {
	return "comment-doc:" + fileType + ":" + fileToken
}

type commentSession struct {
	messages   []map[string]any
	lastAccess float64
}

var (
	commentSessionMu    sync.Mutex
	commentSessionCache = map[string]*commentSession{}
)

// Conversation history of a document session, expiring after an hour without access
func loadSessionHistory(key string) []map[string]any {
	commentSessionMu.Lock()
	defer commentSessionMu.Unlock()
	entry, ok := commentSessionCache[key]
	if ok && nowFn()-entry.lastAccess > sessionTTLSeconds {
		delete(commentSessionCache, key)
		slog.Info("[Feishu-Comment] Session expired: " + key)
		return nil
	}
	if !ok {
		return nil
	}
	entry.lastAccess = nowFn()
	return append([]map[string]any{}, entry.messages...)
}

// Keeps the last user and assistant messages of a document session
func saveSessionHistory(key string, messages []map[string]any) {
	var cleaned []map[string]any
	for _, m := range messages {
		role := pyrepr.Str(m["role"])
		if (role == "user" || role == "assistant") && pyrepr.Truthy(m["content"]) {
			cleaned = append(cleaned, m)
		}
	}
	if len(cleaned) > sessionMaxMessages {
		cleaned = cleaned[len(cleaned)-sessionMaxMessages:]
	}
	commentSessionMu.Lock()
	commentSessionCache[key] = &commentSession{messages: cleaned, lastAccess: nowFn()}
	commentSessionMu.Unlock()
	slog.Info(fmt.Sprintf("[Feishu-Comment] Session saved: %s (%d messages)", key, len(cleaned)))
}

// CommentAgentRun is the outcome of one comment agent turn
type CommentAgentRun struct {
	FinalResponse string
	APICalls      int
	Messages      []map[string]any
}

// CommentAgentRuntime is the model and provider credentials a comment agent turn resolves
type CommentAgentRuntime struct {
	Model    string
	Provider string
	BaseURL  string
}

// CommentAgentHooks log the resolved runtime and load the session history, then log the start of the conversation
type CommentAgentHooks struct {
	OnRuntime func(CommentAgentRuntime) []map[string]any
	OnRun     func()
}

// Resolves the comment agent runtime and runs one turn with the feishu doc and drive toolsets, wired by the gateway runner
var CommentAgentRunner func(ctx context.Context, prompt string, hooks CommentAgentHooks) (*CommentAgentRun, error)

// Runs the comment agent with the lark client injected for the feishu tools, empty on failure
func runCommentAgent(ctx context.Context, prompt string, client larkAPI, sessionKey string) string {
	slog.Info("[Feishu-Comment] _run_comment_agent: injecting lark client into tool thread-locals")
	var turnClient tools.FeishuAPI
	if client != nil {
		turnClient = client
	}
	ctx = tools.WithFeishuClient(ctx, turnClient)
	var history []map[string]any
	hooks := CommentAgentHooks{
		OnRuntime: func(rt CommentAgentRuntime) []map[string]any {
			slog.Info(fmt.Sprintf("[Feishu-Comment] _run_comment_agent: model=%s provider=%s base_url=%s", rt.Model, rt.Provider, pystr.Slice(rt.BaseURL, 0, 50)))
			if sessionKey != "" {
				history = loadSessionHistory(sessionKey)
			}
			if len(history) > 0 {
				slog.Info(fmt.Sprintf("[Feishu-Comment] _run_comment_agent: loaded %d history messages from session %s", len(history), sessionKey))
			}
			return history
		},
		OnRun: func() {
			slog.Info(fmt.Sprintf("[Feishu-Comment] _run_comment_agent: calling run_conversation (prompt=%d chars, history=%d)", pystr.Len(prompt), len(history)))
		},
	}
	run, err := CommentAgentRunner(ctx, prompt, hooks)
	if err != nil {
		slog.Error("[Feishu-Comment] _run_comment_agent: agent failed: " + err.Error())
		return ""
	}
	response := pystr.Strip(run.FinalResponse)
	slog.Info(fmt.Sprintf("[Feishu-Comment] _run_comment_agent: done api_calls=%d response_len=%d response=%s", run.APICalls, pystr.Len(response), pystr.Slice(response, 0, 200)))
	if sessionKey != "" && len(run.Messages) > 0 {
		saveSessionHistory(sessionKey, run.Messages)
	}
	return response
}

// Text and index of the last timeline entry matching pred
func lastIndexWhere(timeline []timelineEntry, pred func(timelineEntry) bool) (string, int, bool) {
	for i := len(timeline) - 1; i >= 0; i-- {
		if pred(timeline[i]) {
			return timeline[i].Text, i, true
		}
	}
	return "", -1, false
}

func newTimelineEntry(r *pyjson.Object, selfOpenID string) timelineEntry {
	uid := getReplyUserID(r)
	return timelineEntry{UserID: uid, Text: extractReplyText(r, false, ""), IsSelf: selfOpenID != "" && uid == selfOpenID}
}

// Prompt for a whole document comment from every whole comment's replies
func wholeCommentPrompt(ctx context.Context, client larkAPI, fromOpenID string, doc commentDoc) string {
	slog.Info("[Feishu-Comment] Fetching whole-document comments for timeline...")
	var replies []any
	for _, wc := range listWholeComments(ctx, client, doc.FileToken, doc.FileType) {
		replies = append(replies, replyListReplies(mustObj(wc))...)
	}
	timeline := make([]timelineEntry, len(replies))
	for i, r := range replies {
		timeline[i] = newTimelineEntry(mustObj(r), doc.SelfOpenID)
	}
	currentText, currentIndex, nearestSelf := "", -1, -1
	for i, r := range replies {
		if timeline[i].UserID == fromOpenID {
			currentText, currentIndex = extractReplyText(mustObj(r), true, doc.SelfOpenID), i
		}
		if timeline[i].IsSelf {
			nearestSelf = i
		}
	}
	if currentText == "" {
		if t, i, ok := lastIndexWhere(timeline, func(e timelineEntry) bool { return !e.IsSelf }); ok {
			currentText, currentIndex = t, i
		}
	}
	shown := "(empty)"
	if currentText != "" {
		shown = pystr.Slice(currentText, 0, 80)
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] Whole timeline: %d entries, current_idx=%d, self_idx=%d, text=%s", len(timeline), currentIndex, nearestSelf, shown))
	return buildWholeCommentPrompt(doc, currentText, timeline, currentIndex, nearestSelf, referencedDocsText(ctx, client, replies, doc.FileToken))
}

// Prompt for a local comment from its thread replies
func localCommentPrompt(ctx context.Context, client larkAPI, commentID, replyID, fromOpenID, quoteText string, doc commentDoc) string {
	slog.Info("[Feishu-Comment] Fetching comment thread replies...")
	replies := listCommentReplies(ctx, client, doc.FileToken, doc.FileType, commentID, replyID)
	timeline := make([]timelineEntry, len(replies))
	for i, r := range replies {
		timeline[i] = newTimelineEntry(mustObj(r), doc.SelfOpenID)
	}
	rootText := ""
	if len(replies) > 0 {
		rootText = extractReplyText(mustObj(replies[0]), true, doc.SelfOpenID)
	}
	targetText, targetIndex := "", -1
	for i, r := range replies {
		if replyID != "" && pyrepr.Equal(dgetDef(mustObj(r), "reply_id", ""), replyID) {
			targetText, targetIndex = extractReplyText(mustObj(r), true, doc.SelfOpenID), i
		}
	}
	if targetText == "" {
		if t, i, ok := lastIndexWhere(timeline, func(e timelineEntry) bool { return e.UserID == fromOpenID }); ok {
			targetText, targetIndex = t, i
		}
	}
	short := func(t string) string {
		if t == "" {
			return "(empty)"
		}
		return pystr.Slice(t, 0, 60)
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] Local timeline: %d entries, target_idx=%d, quote=%s root=%s target=%s", len(timeline), targetIndex,
		short(quoteText), short(rootText), short(targetText)))
	return buildLocalCommentPrompt(doc, commentID, quoteText, rootText, targetText, timeline, targetIndex, referencedDocsText(ctx, client, replies, doc.FileToken))
}

// Runs a drive comment event in its adapter's profile scope, dropping a secondary adapter's unscoped event
func HandleDriveCommentEvent(ctx context.Context, client larkAPI, data any, selfOpenID string, secondary bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			ae, ok := r.(attrError)
			if !ok {
				panic(r)
			}
			err = ae
		}
	}()
	if provider.CurrentSecretScope(ctx) != nil {
		handleDriveCommentEvent(ctx, client, data, selfOpenID)
		return nil
	}
	if secondary && provider.IsMultiplexActive(ctx) {
		slog.Warn("[Feishu-Comment] Dropping unscoped comment event from a secondary profile adapter")
		return nil
	}
	handleDriveCommentEvent(provider.WithLaunchProfileScopeIfMultiplexed(ctx), client, data, selfOpenID)
	return nil
}

// Parses, filters and access checks a comment event, then builds the prompt, runs the agent and delivers the reply
func handleDriveCommentEvent(ctx context.Context, client larkAPI, data any, selfOpenID string) {
	slog.Info("[Feishu-Comment] ========== handle_drive_comment_event START ==========")
	parsed, _, ok := parseDriveCommentEvent(data)
	if !ok {
		slog.Warn("[Feishu-Comment] Dropping malformed drive comment event")
		return
	}
	slog.Info("[Feishu-Comment] [Step 0/5] Event parsed successfully")
	fileToken, fileType, commentID, replyID := parsed["file_token"], parsed["file_type"], parsed["comment_id"], parsed["reply_id"]
	fromOpenID, toOpenID, noticeType := parsed["from_open_id"], parsed["to_open_id"], parsed["notice_type"]
	switch {
	case fromOpenID != "" && selfOpenID != "" && fromOpenID == selfOpenID:
		slog.Debug("[Feishu-Comment] Skipping self-authored event: from=" + fromOpenID)
		return
	case toOpenID == "" || (selfOpenID != "" && toOpenID != selfOpenID):
		slog.Debug("[Feishu-Comment] Skipping event not addressed to self: to=" + firstNonEmpty(toOpenID, "(empty)"))
		return
	case noticeType != "" && !allowedNoticeTypes[noticeType]:
		slog.Debug("[Feishu-Comment] Skipping notice_type=" + noticeType)
		return
	case fileToken == "" || fileType == "" || commentID == "":
		slog.Warn("[Feishu-Comment] Missing required fields, skipping")
		return
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] Event: notice=%s file=%s:%s comment=%s from=%s", noticeType, fileType, fileToken, commentID, fromOpenID))
	cfg, err := LoadConfig(ctx)
	if err != nil {
		panic(attrError{err.Error()})
	}
	rule := ResolveRule(cfg, fileType, fileToken, "")
	if (rule.MatchSource == "wildcard" || rule.MatchSource == "top") && HasWikiKeys(cfg) {
		if wikiToken := reverseLookupWikiToken(ctx, client, fileType, fileToken); wikiToken != "" {
			rule = ResolveRule(cfg, fileType, fileToken, wikiToken)
		}
	}
	if !pyrepr.Truthy(rule.Enabled) {
		slog.Info(fmt.Sprintf("[Feishu-Comment] Comments disabled for %s:%s, skipping", fileType, fileToken))
		return
	}
	allowed, err := IsUserAllowed(ctx, rule, fromOpenID)
	if err != nil {
		panic(attrError{err.Error()})
	}
	if !allowed {
		slog.Info(fmt.Sprintf("[Feishu-Comment] User %s denied (policy=%s, rule=%s)", fromOpenID, rule.Policy, rule.MatchSource))
		return
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] Access granted: user=%s policy=%s rule=%s", fromOpenID, rule.Policy, rule.MatchSource))
	if replyID != "" {
		go func() {
			defer func() { _ = recover() }()
			updateCommentReaction(context.WithoutCancel(ctx), client, "add", fileToken, fileType, replyID, "OK")
		}()
	}
	slog.Info("[Feishu-Comment] [Step 2/5] Parallel fetch: doc meta + comment batch_query")
	var meta, detail *pyjson.Object
	var wg sync.WaitGroup
	var metaPanic, detailPanic any
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { metaPanic = recover() }()
		meta = queryDocumentMeta(ctx, client, fileToken, fileType)
	}()
	go func() {
		defer wg.Done()
		defer func() { detailPanic = recover() }()
		detail = batchQueryComment(ctx, client, fileToken, fileType, commentID)
	}()
	wg.Wait()
	for _, p := range []any{metaPanic, detailPanic} {
		if p != nil {
			panic(p)
		}
	}
	doc := commentDoc{Title: pyrepr.Str(dgetDef(meta, "title", "Untitled")), URL: pyrepr.Str(dgetDef(meta, "url", "")), FileToken: fileToken,
		FileType: fileType, SelfOpenID: selfOpenID}
	isWhole := pyrepr.Truthy(dget(detail, "is_whole"))
	slog.Info(fmt.Sprintf("[Feishu-Comment] Comment context: title=%s is_whole=%s", doc.Title, pyBool(isWhole)))
	slog.Info(fmt.Sprintf("[Feishu-Comment] [Step 3/5] Building timeline (is_whole=%s)", pyBool(isWhole)))
	var prompt string
	if isWhole {
		prompt = wholeCommentPrompt(ctx, client, fromOpenID, doc)
	} else {
		prompt = localCommentPrompt(ctx, client, commentID, replyID, fromOpenID, pyrepr.Str(dgetDef(detail, "quote", "")), doc)
	}
	slog.Info(fmt.Sprintf("[Feishu-Comment] [Step 4/5] Prompt built (%d chars), running agent...", pystr.Len(prompt)))
	slog.Debug("[Feishu-Comment] Full prompt:\n" + prompt)
	response := runCommentAgent(ctx, prompt, client, commentSessionKey(fileType, fileToken))
	if response == "" || strings.Contains(response, noReplySentinel) {
		slog.Info("[Feishu-Comment] Agent returned NO_REPLY, skipping delivery")
	} else {
		slog.Info(fmt.Sprintf("[Feishu-Comment] Agent response (%d chars): %s", pystr.Len(response), pystr.Slice(response, 0, 200)))
		slog.Info(fmt.Sprintf("[Feishu-Comment] [Step 5/5] Delivering reply (is_whole=%s, comment_id=%s)", pyBool(isWhole), commentID))
		if deliverCommentReply(ctx, client, fileToken, fileType, commentID, response, isWhole) {
			slog.Info("[Feishu-Comment] Reply delivered successfully")
		} else {
			slog.Error("[Feishu-Comment] Failed to deliver reply")
		}
	}
	if replyID != "" {
		updateCommentReaction(ctx, client, "delete", fileToken, fileType, replyID, "OK")
	}
	slog.Info("[Feishu-Comment] ========== handle_drive_comment_event END ==========")
}
