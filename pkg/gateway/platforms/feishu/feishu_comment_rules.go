package feishu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Patched rules and pairing file paths, empty follows the active profile home at call time
var (
	RulesFile   string
	PairingFile string
)

var validPolicies = []string{"allowlist", "pairing"}

// StringSet is a frozenset of strings, nil stands for None
type StringSet map[string]struct{}

// NewStringSet builds a non nil set from the given members
func NewStringSet(members ...string) StringSet {
	s := StringSet{}
	for _, m := range members {
		s[m] = struct{}{}
	}
	return s
}

// Has reports set membership
func (s StringSet) Has(m string) bool {
	_, ok := s[m]
	return ok
}

// Sorted lists the members in ascending order
func (s StringSet) Sorted() []string {
	out := make([]string, 0, len(s))
	for m := range s {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

type CommentDocumentRule struct {
	Enabled   *bool
	Policy    *string
	AllowFrom StringSet
}

type CommentsConfig struct {
	Enabled   any
	Policy    string
	AllowFrom StringSet
	Documents map[string]CommentDocumentRule
}

// DefaultCommentsConfig returns the code defaults enabled, pairing policy and empty allowlist
func DefaultCommentsConfig() CommentsConfig {
	return CommentsConfig{Enabled: true, Policy: "pairing", AllowFrom: StringSet{}, Documents: map[string]CommentDocumentRule{}}
}

type ResolvedCommentRule struct {
	Enabled     any
	Policy      string
	AllowFrom   StringSet
	MatchSource string
}

// RulesFilePath is the patched rules file or the active profile rules file
func RulesFilePath(ctx context.Context) string {
	if RulesFile != "" {
		return RulesFile
	}
	return filepath.Join(home.Dir(ctx), "feishu_comment_rules.json")
}

// PairingFilePath is the patched pairing file or the active profile pairing file
func PairingFilePath(ctx context.Context) string {
	if PairingFile != "" {
		return PairingFile
	}
	return filepath.Join(home.Dir(ctx), "feishu_comment_pairing.json")
}

type mtimeEntry struct {
	version []any
	data    *pyjson.Object
}

// MtimeCache serves a JSON object file per resolved path and rereads it only when its cache version changes
type MtimeCache struct {
	resolve func(context.Context) string
	mu      sync.Mutex
	entries map[string]mtimeEntry
}

// NewMtimeCache caches a fixed path
func NewMtimeCache(path string) *MtimeCache {
	return NewMtimeCacheFunc(func(context.Context) string { return path })
}

// NewMtimeCacheFunc caches whatever path the resolver returns at each access
func NewMtimeCacheFunc(resolve func(context.Context) string) *MtimeCache {
	return &MtimeCache{resolve: resolve, entries: map[string]mtimeEntry{}}
}

// Invalidate drops the slot of the currently resolved path
func (c *MtimeCache) Invalidate(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, c.resolve(ctx))
}

// Load returns the cached object, an empty object for a missing or unreadable file, and an error for invalid UTF-8
func (c *MtimeCache) Load(ctx context.Context) (*pyjson.Object, error) {
	path := c.resolve(ctx)
	version, verr := config.FileCacheVersion(path, false)
	c.mu.Lock()
	defer c.mu.Unlock()
	if verr != nil && errors.Is(verr, fs.ErrNotExist) {
		delete(c.entries, path)
		return pyjson.NewObject(), nil
	}
	if verr != nil {
		version = nil
	}
	if cached, ok := c.entries[path]; ok && version != nil && reflect.DeepEqual(cached.version, version) {
		return cached.data, nil
	}
	data, err := readJSONObject(path)
	if err != nil {
		return nil, err
	}
	c.entries[path] = mtimeEntry{version: version, data: data}
	return data, nil
}

// Reads a utf-8-sig JSON file, logging and returning an empty object on read or decode failure
func readJSONObject(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	var value any
	if err == nil {
		text, derr := pystr.DecodeUTF8Sig(raw)
		if derr != nil {
			return nil, derr
		}
		value, err = pyjson.Loads(text)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[Feishu-Rules] Failed to read %s, using empty config", path))
		return pyjson.NewObject(), nil
	}
	if obj, ok := value.(*pyjson.Object); ok {
		return obj, nil
	}
	return pyjson.NewObject(), nil
}

var (
	rulesCache   = NewMtimeCacheFunc(RulesFilePath)
	pairingCache = NewMtimeCacheFunc(PairingFilePath)
)

// Parses a list of strings into a set of stripped non empty members, nil when the value is not a list
func parseFrozenset(raw any) StringSet {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := StringSet{}
	for _, u := range items {
		if s := pystr.Strip(pyrepr.Str(u)); s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

// Normalizes a policy value, unknown values fall back to the default
func parsePolicy(raw any, def *string) *string {
	if raw == nil {
		return def
	}
	policy := pystr.Lower(pystr.Strip(pyrepr.Str(raw)))
	for _, p := range validPolicies {
		if p == policy {
			return &policy
		}
	}
	return def
}

// Builds a per document rule where absent fields inherit from the lower tier
func parseDocumentRule(raw *pyjson.Object) CommentDocumentRule {
	rule := CommentDocumentRule{Policy: parsePolicy(objGet(raw, "policy", nil), nil), AllowFrom: parseFrozenset(objGet(raw, "allow_from", nil))}
	if enabled := objGet(raw, "enabled", nil); enabled != nil {
		b := pyrepr.Truthy(enabled)
		rule.Enabled = &b
	}
	return rule
}

func objGet(o *pyjson.Object, key string, def any) any {
	if v, ok := o.Vals[key]; ok {
		return v
	}
	return def
}

// LoadConfig reads the comment rules of the active profile through the mtime cache
func LoadConfig(ctx context.Context) (CommentsConfig, error) {
	raw, err := rulesCache.Load(ctx)
	if err != nil {
		return CommentsConfig{}, err
	}
	if raw.Len() == 0 {
		return DefaultCommentsConfig(), nil
	}
	documents := map[string]CommentDocumentRule{}
	if docs, ok := objGet(raw, "documents", pyjson.NewObject()).(*pyjson.Object); ok {
		for _, key := range docs.Keys {
			if ruleRaw, ok := docs.Vals[key].(*pyjson.Object); ok {
				documents[key] = parseDocumentRule(ruleRaw)
			}
		}
	}
	pairing := "pairing"
	allow := parseFrozenset(objGet(raw, "allow_from", nil))
	if len(allow) == 0 {
		allow = StringSet{}
	}
	return CommentsConfig{
		Enabled: objGet(raw, "enabled", true), Policy: *parsePolicy(objGet(raw, "policy", "pairing"), &pairing),
		AllowFrom: allow, Documents: documents,
	}, nil
}

// HasWikiKeys reports whether any document rule key starts with wiki:
func HasWikiKeys(cfg CommentsConfig) bool {
	for k := range cfg.Documents {
		if strings.HasPrefix(k, "wiki:") {
			return true
		}
	}
	return false
}

// ResolveRule picks each field from the exact or wiki rule, then the wildcard rule, then the top level
func ResolveRule(cfg CommentsConfig, fileType, fileToken, wikiToken string) ResolvedCommentRule {
	exactKey := fileType + ":" + fileToken
	exact, hasExact := cfg.Documents[exactKey]
	if !hasExact && wikiToken != "" {
		exactKey = "wiki:" + wikiToken
		exact, hasExact = cfg.Documents[exactKey]
	}
	wildcard, hasWildcard := cfg.Documents["*"]
	type layer struct {
		rule CommentDocumentRule
		ok   bool
		src  string
	}
	layers := []layer{{exact, hasExact, "exact:" + exactKey}, {wildcard, hasWildcard, "wildcard"}}
	enabled, enSrc := cfg.Enabled, "top"
	for _, l := range layers {
		if l.ok && l.rule.Enabled != nil {
			enabled, enSrc = *l.rule.Enabled, l.src
			break
		}
	}
	policy, polSrc := cfg.Policy, "top"
	for _, l := range layers {
		if l.ok && l.rule.Policy != nil {
			policy, polSrc = *l.rule.Policy, l.src
			break
		}
	}
	allow := cfg.AllowFrom
	for _, l := range layers {
		if l.ok && l.rule.AllowFrom != nil {
			allow = l.rule.AllowFrom
			break
		}
	}
	priority := func(src string) int {
		switch strings.SplitN(src, ":", 2)[0] {
		case "exact":
			return 0
		case "wildcard":
			return 1
		case "top":
			return 2
		}
		return 3
	}
	best := enSrc
	if priority(polSrc) < priority(enSrc) {
		best = polSrc
	}
	return ResolvedCommentRule{Enabled: enabled, Policy: policy, AllowFrom: allow, MatchSource: best}
}

// Approved user open_ids of the pairing store, read through the mtime cache
var loadPairingApproved = func(ctx context.Context) (StringSet, error) {
	data, err := pairingCache.Load(ctx)
	if err != nil {
		return nil, err
	}
	out := StringSet{}
	switch approved := objGet(data, "approved", pyjson.NewObject()).(type) {
	case *pyjson.Object:
		for _, k := range approved.Keys {
			out[k] = struct{}{}
		}
	case []any:
		for _, u := range approved {
			if pyrepr.Truthy(u) {
				out[pyrepr.Str(u)] = struct{}{}
			}
		}
	}
	return out, nil
}

// Writes the pairing store atomically through a .tmp sibling and drops its cache slot
func savePairing(ctx context.Context, data *pyjson.Object) error {
	pairingFile := PairingFilePath(ctx)
	if err := os.MkdirAll(filepath.Dir(pairingFile), 0o777); err != nil {
		return err
	}
	text, err := pyjson.Dumps(data, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	tmp := strings.TrimSuffix(pairingFile, filepath.Ext(pairingFile)) + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o666); err != nil {
		return err
	}
	if err := os.Rename(tmp, pairingFile); err != nil {
		return err
	}
	pairingCache.Invalidate(ctx)
	return nil
}

// Adds or removes a user in the approved dict and reports whether the store changed
func mutatePairing(ctx context.Context, userOpenID string, add bool) (bool, error) {
	data, err := pairingCache.Load(ctx)
	if err != nil {
		return false, err
	}
	approved, ok := objGet(data, "approved", nil).(*pyjson.Object)
	if !ok {
		approved = pyjson.NewObject()
	}
	_, present := approved.Vals[userOpenID]
	if present == add {
		return false, nil
	}
	if add {
		entry := pyjson.NewObject()
		entry.Set("approved_at", float64(time.Now().UnixNano())/1e9)
		approved.Set(userOpenID, entry)
	} else {
		approved.Delete(userOpenID)
	}
	data.Set("approved", approved)
	if err := savePairing(ctx, data); err != nil {
		return false, err
	}
	return true, nil
}

// PairingAdd approves a user and reports whether it was newly added
func PairingAdd(ctx context.Context, userOpenID string) (bool, error) {
	return mutatePairing(ctx, userOpenID, true)
}

// PairingRemove revokes a user and reports whether it was removed
func PairingRemove(ctx context.Context, userOpenID string) (bool, error) {
	return mutatePairing(ctx, userOpenID, false)
}

// PairingList returns a copy of the approved dict keyed by user open_id
func PairingList(ctx context.Context) (*pyjson.Object, error) {
	data, err := pairingCache.Load(ctx)
	if err != nil {
		return nil, err
	}
	out := pyjson.NewObject()
	if approved, ok := objGet(data, "approved", pyjson.NewObject()).(*pyjson.Object); ok {
		for _, k := range approved.Keys {
			out.Set(k, approved.Vals[k])
		}
	}
	return out, nil
}

// IsUserAllowed passes users on the allowlist and, under the pairing policy, users in the pairing store
func IsUserAllowed(ctx context.Context, rule ResolvedCommentRule, userOpenID string) (bool, error) {
	if rule.AllowFrom.Has(userOpenID) {
		return true, nil
	}
	if rule.Policy != "pairing" {
		return false, nil
	}
	approved, err := loadPairingApproved(ctx)
	if err != nil {
		return false, err
	}
	return approved.Has(userOpenID), nil
}

func listRepr(members []string) string {
	items := make([]any, len(members))
	for i, m := range members {
		items[i] = m
	}
	return pyrepr.Repr(items)
}

func fmtAllow(allow StringSet) string {
	if len(allow) == 0 {
		return "[]"
	}
	return listRepr(allow.Sorted())
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Reads a key of an approved entry the way dict.get does, failing like Python on a non dict entry
func metaGet(meta any, key string, def any) (any, error) {
	obj, ok := meta.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(meta))
	}
	return objGet(obj, key, def), nil
}

// Prints the rules config, every document rule and the pairing approvals
func printStatus(ctx context.Context, w io.Writer) error {
	cfg, err := LoadConfig(ctx)
	if err != nil {
		return err
	}
	rulesFile, pairingFile := RulesFilePath(ctx), PairingFilePath(ctx)
	fmt.Fprintf(w, "Rules file: %s\n  exists: %s\nPairing file: %s\n  exists: %s\n\n", rulesFile, pyrepr.Str(pathExists(rulesFile)), pairingFile, pyrepr.Str(pathExists(pairingFile)))
	fmt.Fprintf(w, "Top-level:\n  enabled:    %s\n  policy:     %s\n  allow_from: %s\n\n", pyrepr.Str(cfg.Enabled), cfg.Policy, fmtAllow(cfg.AllowFrom))
	if len(cfg.Documents) > 0 {
		fmt.Fprintf(w, "Document rules (%d):\n", len(cfg.Documents))
	} else {
		fmt.Fprintln(w, "Document rules: (none)")
	}
	keys := make([]string, 0, len(cfg.Documents))
	for k := range cfg.Documents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rule := cfg.Documents[key]
		var parts []string
		if rule.Enabled != nil {
			parts = append(parts, "enabled="+pyrepr.Str(*rule.Enabled))
		}
		if rule.Policy != nil {
			parts = append(parts, "policy="+*rule.Policy)
		}
		if rule.AllowFrom != nil {
			parts = append(parts, "allow_from="+listRepr(rule.AllowFrom.Sorted()))
		}
		body := "(empty — inherits all)"
		if len(parts) > 0 {
			body = strings.Join(parts, ", ")
		}
		fmt.Fprintf(w, "  [%s] %s\n", key, body)
	}
	fmt.Fprintln(w)
	approved, err := PairingList(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Pairing approved (%d):\n", approved.Len())
	uids := append([]string(nil), approved.Keys...)
	sort.Strings(uids)
	for _, uid := range uids {
		at, err := metaGet(approved.Vals[uid], "approved_at", int64(0))
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %s  (approved_at=%s)\n", uid, pyrepr.Str(at))
	}
	return nil
}

// Simulates the access check of one user on one fileType:token document
func doCheck(ctx context.Context, w io.Writer, docKey, userOpenID string) error {
	parts := strings.SplitN(docKey, ":", 2)
	if len(parts) != 2 {
		fmt.Fprintf(w, "Error: doc_key must be 'fileType:fileToken', got '%s'\n", docKey)
		return nil
	}
	cfg, err := LoadConfig(ctx)
	if err != nil {
		return err
	}
	rule := ResolveRule(cfg, parts[0], parts[1], "")
	allowed, err := IsUserAllowed(ctx, rule, userOpenID)
	if err != nil {
		return err
	}
	result := "DENIED"
	if allowed {
		result = "ALLOWED"
	}
	fmt.Fprintf(w, "Document:     %s\nUser:         %s\nResolved rule:\n  enabled:      %s\n  policy:       %s\n", docKey, userOpenID, pyrepr.Str(rule.Enabled), rule.Policy)
	fmt.Fprintf(w, "  allow_from:   %s\n  match_source: %s\nResult:       %s\n", fmtAllow(rule.AllowFrom), rule.MatchSource, result)
	return nil
}

type pairingOp struct {
	fn          func(context.Context, string) (bool, error)
	okMsg, noop string
}

var pairingOps = map[string]pairingOp{
	"add":    {PairingAdd, "Added: %s", "Already approved: %s"},
	"remove": {PairingRemove, "Removed: %s", "Not in approved list: %s"},
}

// Handles pairing add, remove and list and returns the exit code
func pairingCmd(ctx context.Context, w io.Writer, args []string) (int, error) {
	sub, hasSub := "", len(args) > 1
	if hasSub {
		sub = args[1]
	}
	if hasSub && sub == "list" {
		approved, err := PairingList(ctx)
		if err != nil {
			return 0, err
		}
		if approved.Len() == 0 {
			fmt.Fprintln(w, "(no approved users)")
			return 0, nil
		}
		uids := append([]string(nil), approved.Keys...)
		sort.Strings(uids)
		lines := make([]string, 0, len(uids))
		for _, uid := range uids {
			at, err := metaGet(approved.Vals[uid], "approved_at", "?")
			if err != nil {
				return 0, err
			}
			lines = append(lines, fmt.Sprintf("  %s  approved_at=%s", uid, pyrepr.Str(at)))
		}
		fmt.Fprintln(w, strings.Join(lines, "\n"))
		return 0, nil
	}
	op, known := pairingOps[sub]
	if hasSub && known && len(args) >= 3 {
		changed, err := op.fn(ctx, args[2])
		if err != nil {
			return 0, err
		}
		msg := op.noop
		if changed {
			msg = op.okMsg
		}
		fmt.Fprintf(w, msg+"\n", args[2])
		return 0, nil
	}
	switch {
	case !hasSub:
		fmt.Fprintln(w, "Usage: pairing <add|remove|list> [args]")
	case known:
		fmt.Fprintf(w, "Usage: pairing %s <user_open_id>\n", sub)
	default:
		fmt.Fprintf(w, "Unknown pairing subcommand: %s\n", sub)
	}
	return 1, nil
}

// Main runs the status, check and pairing commands over args without the program name and returns the exit code
func Main(ctx context.Context, args []string, w io.Writer) (int, error) {
	_, _ = provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{})
	usage := fmt.Sprintf(`Usage: python -m gateway.platforms.feishu_comment_rules <command> [args]

Commands:
  status                              Show rules config and pairing state
  check <fileType:token> <user>        Simulate access check
  pairing add <user_open_id>           Add user to pairing-approved list
  pairing remove <user_open_id>        Remove user from pairing-approved list
  pairing list                         List pairing-approved users

Rules config file: %s
  Edit this JSON file directly to configure policies and document rules.
  Changes take effect on the next comment event (no restart needed).
`, RulesFilePath(ctx))
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch {
	case cmd == "status":
		if err := printStatus(ctx, w); err != nil {
			return 0, err
		}
	case cmd == "check" && len(args) >= 3:
		if err := doCheck(ctx, w, args[1], args[2]); err != nil {
			return 0, err
		}
	case cmd == "check":
		fmt.Fprintln(w, "Usage: check <fileType:fileToken> <user_open_id>")
		return 1, nil
	case cmd == "pairing":
		return pairingCmd(ctx, w, args)
	default:
		if cmd != "" {
			fmt.Fprintf(w, "Unknown command: %s\n%s\n", cmd, usage)
		} else {
			fmt.Fprintln(w, usage)
		}
		return 1, nil
	}
	return 0, nil
}
