package hooks

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Hook as a person adds it: the event it runs on, its command, the tools it is for, the seconds it may take and whether its failure blocks
type Hook struct {
	Event, Command, Matcher string
	Timeout                 int
	Block                   bool
}

// Fields an entry of a hook may hold, and those of a webhook
var (
	hookFields    = []string{shellconfig.Command, shellconfig.Matcher, shellconfig.Timeout, shellconfig.Block}
	webhookFields = []string{shellconfig.URL, shellconfig.Posted, shellconfig.Name, shellconfig.Matcher, shellconfig.Timeout, shellconfig.Secret,
		shellconfig.SecretEnv}
)

// State of a hook in the words every page and command shows: allowed by you or by Run without asking, allowed but its script changed since,
// or not allowed
func (v View) State(acceptAll bool) string {
	switch {
	case v.Allowed && v.Drift == 1:
		return "Changed since you allowed it"
	case v.Allowed, acceptAll:
		return "Allowed"
	}
	return "Not allowed"
}

// State of a webhook in the words every page and command shows
func (t *Target) State() string {
	if t.Secret != "" {
		return "Signed"
	}
	return "Not signed"
}

// The event as a person reads it
func title(event string) string { return shellconfig.PointOf(event).Title }

// Hooks of the event as the profile keeps them, a copy that can be changed
func hooksOf(cfg *config.Config, event string) []any {
	block, _ := cfg.Get(shellconfig.Events).(map[string]any)
	list, _ := block[event].([]any)
	return slices.Clone(list)
}

// Place of the hook that runs the command in the list, -1 for none
func placeOf(list []any, command string) int {
	return slices.IndexFunc(list, func(item any) bool {
		entry, _ := item.(map[string]any)
		text, _ := entry[shellconfig.Command].(string)
		return strings.TrimSpace(text) == command
	})
}

// Command of a hook as the settings keep it, for one a person names: the hook of the event that runs that very text, else the one that
// runs it once a leading ~ is the home folder, which a shell makes of it before a command of icebath reads it. With no event every event is
// looked in, then the hooks that were allowed. The text itself when no hook runs it
func (r *Registrant) Stored(cfg *config.Config, event, command string) string {
	command = strings.TrimSpace(command)
	var kept []string
	for _, spec := range r.Configured(cfg) {
		if event == "" || spec.Event == event {
			kept = append(kept, spec.Command)
		}
	}
	if event == "" {
		allowed, _ := r.allowed(cfg)
		for _, consent := range slices.SortedFunc(maps.Keys(allowed), func(a, b pair) int { return strings.Compare(a.command, b.command) }) {
			kept = append(kept, consent.command)
		}
	}
	if slices.Contains(kept, command) {
		return command
	}
	home := fsx.ExpandHome(command)
	for _, each := range kept {
		if fsx.ExpandHome(each) == home {
			return each
		}
	}
	return command
}

// Entry of a hook as the settings keep it, its fields left out where they hold their defaults
func (h Hook) entry() map[string]any {
	entry := map[string]any{shellconfig.Command: strings.TrimSpace(h.Command)}
	if matcher := strings.TrimSpace(h.Matcher); matcher != "" {
		entry[shellconfig.Matcher] = matcher
	}
	if h.Timeout > 0 {
		entry[shellconfig.Timeout] = h.Timeout
	}
	if h.Block {
		entry[shellconfig.Block] = true
	}
	return entry
}

// Adds a hook to the end of its event's list, allowed at once when the person who adds it says so; what was done, in a sentence or two
func (r *Registrant) AddHook(cfg *config.Config, h Hook, allow bool) (string, error) {
	h.Command = strings.TrimSpace(h.Command)
	list := hooksOf(cfg, h.Event)
	if placeOf(list, h.Command) >= 0 {
		return "", &Refusal{Message: "There is a hook running " + h.Command + " on " + title(h.Event) + " already."}
	}
	if err := r.keepEvent(cfg, h.Event, append(list, h.entry())); err != nil {
		return "", err
	}
	said := "Added " + h.Command + " to " + title(h.Event) + "."
	switch {
	case allow:
		if err := r.record(cfg, h.Event, h.Command); err != nil {
			return "", err
		}
	case !cfg.GetBool(shellconfig.AutoAccept):
		said += " It runs once you allow it."
	}
	return said, nil
}

// Changes fields of one hook, a field given nil going back to its default; a hook given another command keeps the consent it had. What was
// done, in a sentence or more
func (r *Registrant) EditHook(cfg *config.Config, event, command string, fields map[string]any) (string, error) {
	list := hooksOf(cfg, event)
	at := placeOf(list, command)
	if at < 0 {
		return "", &Refusal{Message: "There is no hook " + command + " on " + title(event) + "."}
	}
	old, _ := list[at].(map[string]any)
	entry := maps.Clone(old)
	for field, value := range fields {
		if !slices.Contains(hookFields, field) {
			return "", &Refusal{Message: "A hook has no setting called " + field + "."}
		}
		if text, ok := value.(string); ok && field != shellconfig.Command {
			value = strings.TrimSpace(text)
		}
		if value == nil || value == "" && field != shellconfig.Command {
			delete(entry, field)
			continue
		}
		entry[field] = value
	}
	to, _ := entry[shellconfig.Command].(string)
	to = strings.TrimSpace(to)
	entry[shellconfig.Command] = to
	if to != command && placeOf(list, to) >= 0 {
		return "", &Refusal{Message: "There is a hook running " + to + " on " + title(event) + " already."}
	}
	list[at] = entry
	if err := r.keepEvent(cfg, event, list); err != nil {
		return "", err
	}
	if to != command {
		allowed, _ := r.allowed(cfg)
		if allowed[pair{event, command}] {
			if err := r.record(cfg, event, to); err != nil {
				return "", err
			}
		}
		if err := r.forgetPair(cfg, event, command); err != nil {
			return "", err
		}
	}
	return edited(event, command, to, fields, entry), nil
}

// What a change of a hook's fields did, one sentence for each field changed
func edited(event, command, to string, fields map[string]any, entry map[string]any) string {
	var said []string
	for _, field := range hookFields {
		if _, changed := fields[field]; !changed {
			continue
		}
		switch field {
		case shellconfig.Command:
			if to != command {
				said = append(said, "The hook now runs "+to+".")
			}
		case shellconfig.Matcher:
			matcher, _ := entry[shellconfig.Matcher].(string)
			if matcher == "" {
				said = append(said, to+" now runs for every tool.")
			} else {
				said = append(said, to+" now runs for "+ToolWords(matcher)+".")
			}
		case shellconfig.Timeout:
			seconds := DefaultTimeout
			if n, whole := config.Whole(entry[shellconfig.Timeout]); whole {
				seconds = n
			}
			said = append(said, to+" may now take "+strs.Span(time.Duration(seconds)*time.Second)+".")
		case shellconfig.Block:
			if on, _ := entry[shellconfig.Block].(bool); on {
				said = append(said, to+" now blocks the tool call when it fails.")
			} else {
				said = append(said, to+" no longer blocks the tool call when it fails.")
			}
		}
	}
	if len(said) == 0 {
		return "Nothing about " + to + " on " + title(event) + " changed."
	}
	return strings.Join(said, " ")
}

// Tools a pattern names, as a person reads them: the names it holds joined, or the pattern itself when it is more than names
func ToolWords(pattern string) string {
	parts := strings.Split(pattern, "|")
	for _, part := range parts {
		if !plainName.MatchString(part) {
			return pattern
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// Name of one tool, as a pattern that names tools one by one holds it
var plainName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Takes every hook running the command out of the event's list, and the consent given to it; how many went
func (r *Registrant) Remove(cfg *config.Config, event, command string) (int, error) {
	list := hooksOf(cfg, event)
	kept := slices.DeleteFunc(slices.Clone(list), func(item any) bool { return placeOf([]any{item}, command) == 0 })
	removed := len(list) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	var value []any
	if len(kept) > 0 {
		value = kept
	}
	if err := r.keepEvent(cfg, event, value); err != nil {
		return 0, err
	}
	return removed, r.forgetPair(cfg, event, command)
}

// Takes the consent given to the hook of the event that runs the command off the allowlist
func (r *Registrant) forgetPair(cfg *config.Config, event, command string) error {
	return r.update(cfg, func(doc map[string]any) {
		list, _ := doc["approvals"].([]any)
		doc["approvals"] = slices.DeleteFunc(slices.Clone(list), func(item any) bool { return matches(item, &event, command) })
	})
}

// Why an entry of a hook can't be kept, nil when it can: an event no shell hook runs on, a field no hook has, no command, tools on an event
// that is not about a tool, a pattern that can't be read, a time limit out of bounds, or blocking on an event that can't be blocked
func checkHook(event string, raw any) error {
	point, known := hooks.PointOf(event)
	if !known || point.NoShell {
		return &Refusal{Message: "icebath has no hook event “" + event + "”."}
	}
	entry, ok := raw.(map[string]any)
	if !ok {
		return &Refusal{Message: "A hook is a set of named values, such as {command: ~/hooks/check.sh}."}
	}
	for field := range entry {
		if !slices.Contains(hookFields, field) {
			return &Refusal{Message: "A hook has no setting called " + field + "."}
		}
	}
	if command, _ := entry[shellconfig.Command].(string); strings.TrimSpace(command) == "" {
		return &Refusal{Message: "A hook needs a command, such as ~/hooks/check.sh."}
	}
	if err := checkMatcher(entry[shellconfig.Matcher], point.Tool, "Only a hook of "+title(hooks.PreToolCall)+" or "+title(hooks.PostToolCall)+
		" can name tools, and this one runs on "+title(event)+"."); err != nil {
		return err
	}
	if err := checkTimeout(entry[shellconfig.Timeout], MaxTimeout, "A hook"); err != nil {
		return err
	}
	if value, given := entry[shellconfig.Block]; given && value != nil {
		on, ok := value.(bool)
		switch {
		case !ok:
			return &Refusal{Message: "Block when it fails takes on or off."}
		case on && !point.Blocking:
			return &Refusal{Message: "Only a hook of " + title(hooks.PreToolCall) + " can block what it runs on, and this one runs on " + title(event) + "."}
		}
	}
	return nil
}

// Why a pattern of tools can't be kept, nil when it can: not text, given where no event is about a tool, or not a pattern
func checkMatcher(value any, tools bool, elsewhere string) error {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	switch {
	case !ok:
		return &Refusal{Message: "Tools is a pattern of tool names, such as terminal|write_file."}
	case strings.TrimSpace(text) == "":
		return nil
	case !tools:
		return &Refusal{Message: elsewhere}
	}
	if _, err := regexp2.Compile(`\A(?:`+strings.TrimSpace(text)+`)\z`, regexp2.None); err != nil {
		return &Refusal{Message: "“" + text + "” is not a pattern of tool names. Write names joined by |, such as terminal|write_file."}
	}
	return nil
}

// Why a time limit in seconds can't be kept, nil when it can
func checkTimeout(value any, most int, what string) error {
	if value == nil {
		return nil
	}
	if n, whole := config.Whole(value); !whole || n < 1 || n > most {
		return &Refusal{Message: what + " may take from 1 second to " + strs.Span(time.Duration(most)*time.Second) + "."}
	}
	return nil
}

// Why the hooks of an event can't be kept, nil when they can: each checks out and no two run the same command
func checkList(event string, value any) error {
	if value == nil {
		return nil
	}
	list, ok := value.([]any)
	if !ok {
		return &Refusal{Message: "The hooks of " + title(event) + " are a list of hooks."}
	}
	seen := map[string]bool{}
	for _, item := range list {
		if err := checkHook(event, item); err != nil {
			return err
		}
		entry, _ := item.(map[string]any)
		command, _ := entry[shellconfig.Command].(string)
		if command = strings.TrimSpace(command); seen[command] {
			return &Refusal{Message: "There is a hook running " + command + " on " + title(event) + " already."}
		}
		seen[command] = true
	}
	return nil
}

// Keeps the hooks of an event once they check out, none taking the event out: what adding, changing and removing a hook end in, and every
// write of the setting a person asks for
func (r *Registrant) keepEvent(cfg *config.Config, event string, list []any) error {
	if err := checkList(event, list); err != nil {
		return err
	}
	var value any
	if len(list) > 0 {
		value = list
	}
	return cfg.Set(shellconfig.Events+"."+event, value)
}

// Event and the path inside its list of a key under the events setting, both empty for the whole setting. The key comes as the settings
// keep it: the setting spelled as declared, the event as written
func eventAt(key string) (event, path string) {
	rest := strings.TrimPrefix(strings.TrimPrefix(key, shellconfig.Events), ".")
	event, path, _ = strings.Cut(rest, ".")
	return event, path
}

// Why a write of a key under the events setting would be refused, nil when it would be kept
func (r *Registrant) checkEvents(_ *config.Config, key string, value any) error {
	event, path := eventAt(key)
	switch {
	case path != "":
		return &Refusal{Message: "A hook is changed as a whole, with the list of its event."}
	case event != "":
		return checkList(event, value)
	case value == nil:
		return nil
	}
	table, ok := value.(map[string]any)
	if !ok {
		return &Refusal{Message: "Hooks are kept by event, each event with a list of hooks."}
	}
	for name, list := range table {
		if err := checkList(name, list); err != nil {
			return err
		}
	}
	return nil
}

// Makes a write a person asked for of a key under the events setting through the function of what it changes
func (r *Registrant) writeEvents(cfg *config.Config, key string, value any) (bool, error) {
	if err := r.checkEvents(cfg, key, value); err != nil {
		return true, err
	}
	if event, _ := eventAt(key); event != "" {
		list, _ := value.([]any)
		return true, r.keepEvent(cfg, event, list)
	}
	return true, cfg.Set(shellconfig.Events, value)
}

// Stored entry of a webhook, a copy that can be changed; nil for none
func webhookEntry(cfg *config.Config, key string) map[string]any {
	table, _ := cfg.Get(shellconfig.Outbound).(map[string]any)
	entry, ok := table[key].(map[string]any)
	if !ok {
		return nil
	}
	return maps.Clone(entry)
}

// Key a new webhook to the address is kept under: the address's host in words a key holds, a number after it when another has it
func webhookKey(cfg *config.Config, address string) string {
	base := "webhook"
	if parsed, err := url.Parse(address); err == nil && parsed.Hostname() != "" {
		base = strings.Trim(nonName.ReplaceAllString(strings.ToLower(parsed.Hostname()), "-"), "-")
	}
	if base == "" {
		base = "webhook"
	}
	table, _ := cfg.Get(shellconfig.Outbound).(map[string]any)
	key := base
	for n := 2; table[key] != nil; n++ {
		key = base + "-" + strconv.Itoa(n)
	}
	return key
}

// Runs of what a key never holds
var nonName = regexp.MustCompile(`[^a-z0-9_-]+`)

// Items of a list value as values of a list the settings keep
func items(value any) []any {
	switch list := value.(type) {
	case []any:
		return list
	case []string:
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = item
		}
		return out
	}
	return nil
}

// Adds a webhook that posts the events to the address; the key it is kept under, and what was done in a sentence
func (r *Registrant) AddWebhook(cfg *config.Config, address string, events []string) (string, string, error) {
	address = strings.TrimSpace(address)
	entry := map[string]any{shellconfig.URL: address, shellconfig.Posted: items(events)}
	if err := checkWebhook(entry); err != nil {
		return "", "", err
	}
	key := webhookKey(cfg, address)
	if err := cfg.SetNamed(shellconfig.Outbound, entry, key); err != nil {
		return "", "", err
	}
	return key, "Added the webhook to " + address + ". It posts " + strs.Count(len(events), "event") + ".", nil
}

// Changes one field of a webhook, nil putting the default back; what was done, in a sentence
func (r *Registrant) EditWebhook(cfg *config.Config, key, field string, value any) (string, error) {
	entry := webhookEntry(cfg, key)
	switch {
	case entry == nil:
		return "", &Refusal{Message: "There is no webhook " + key + "."}
	case !slices.Contains(webhookFields, field):
		return "", &Refusal{Message: "A webhook has no setting called " + field + "."}
	}
	if list := items(value); list != nil {
		value = list
	}
	if text, ok := value.(string); ok {
		value = strings.TrimSpace(text)
	}
	// A time limit of 0 is the one a webhook has without one
	if n, whole := config.Whole(value); field == shellconfig.Timeout && whole && n == 0 {
		value = nil
	}
	if value == nil || value == "" {
		delete(entry, field)
		value = nil
	} else {
		entry[field] = value
	}
	if err := checkWebhook(entry); err != nil {
		return "", err
	}
	// The key is one name whatever it holds: one a person wrote in the file may hold a dot
	if err := cfg.SetNamed(shellconfig.Outbound, value, key, field); err != nil {
		return "", err
	}
	return webhookEdited(entry, field), nil
}

// What a change of one field of a webhook did
func webhookEdited(entry map[string]any, field string) string {
	address, _ := entry[shellconfig.URL].(string)
	switch field {
	case shellconfig.URL:
		return "The webhook now posts to " + address + "."
	case shellconfig.Posted:
		return "The webhook to " + address + " now posts " + strs.Count(len(items(entry[shellconfig.Posted])), "event") + "."
	case shellconfig.Name:
		if name, _ := entry[shellconfig.Name].(string); name != "" {
			return "The webhook to " + address + " is now called " + name + "."
		}
		return "The webhook to " + address + " is now listed by its address."
	case shellconfig.Matcher:
		if matcher, _ := entry[shellconfig.Matcher].(string); matcher != "" {
			return "The webhook to " + address + " now posts for " + ToolWords(matcher) + "."
		}
		return "The webhook to " + address + " now posts for every tool."
	case shellconfig.Timeout:
		seconds := DefaultPostTimeout
		if n, whole := config.Whole(entry[shellconfig.Timeout]); whole {
			seconds = n
		}
		return "A post to " + address + " may now take " + strs.Span(time.Duration(seconds)*time.Second) + "."
	case shellconfig.SecretEnv:
		if variable, _ := entry[shellconfig.SecretEnv].(string); variable != "" {
			return "Posts to " + address + " are now signed with the secret in " + variable + "."
		}
	}
	// While a variable is named its secret alone signs the posts, whatever secret is kept here
	if variable, _ := entry[shellconfig.SecretEnv].(string); variable != "" {
		return "Posts to " + address + " are signed with the secret in " + variable + ", which is used in place of a secret kept here."
	}
	if secret, _ := entry[shellconfig.Secret].(string); secret != "" {
		return "Posts to " + address + " are now signed with your secret."
	}
	return "Posts to " + address + " are no longer signed."
}

// Takes a webhook out of the settings; what was done, in a sentence
func (r *Registrant) RemoveWebhook(cfg *config.Config, key string) (string, error) {
	entry := webhookEntry(cfg, key)
	if entry == nil {
		return "", &Refusal{Message: "There is no webhook " + key + "."}
	}
	if err := cfg.SetNamed(shellconfig.Outbound, nil, key); err != nil {
		return "", err
	}
	address, _ := entry[shellconfig.URL].(string)
	return "Removed the webhook to " + address + ".", nil
}

// Posts a sample of the webhook's first event to its address now, signed as its posts are: whether the address took it, the status it
// answered with, and how it went in a sentence or two
func (r *Registrant) TestWebhook(ctx context.Context, cfg *config.Config, key string) (bool, int, string, error) {
	target := r.Target(cfg, key)
	if target == nil {
		return false, 0, "", &Refusal{Message: "There is no webhook " + key + " that can post."}
	}
	event := target.Events[0]
	id := strs.Random(16)
	text, err := postBody(ctx, cfg, event, Sample(event, ""), id)
	if err != nil {
		return false, 0, "", err
	}
	status, _, err := r.outbound.send(delivery(event, target, []byte(text), id))
	switch {
	case err != nil:
		return false, 0, "The sample could not reach " + target.URL + ". The error was: " + strings.TrimSuffix(err.Error(), ".") + ".", nil
	case status >= 200 && status < 300:
		return true, status, fmt.Sprintf("Posted a sample of %s to %s, which answered %d %s.", title(event), target.URL, status, http.StatusText(status)), nil
	}
	return false, status, fmt.Sprintf("%s answered the sample of %s with %d %s.", target.URL, title(event), status, http.StatusText(status)), nil
}

// Why an entry of a webhook can't be kept, nil when it can: a field no webhook has, an address that is no web address, no events or one
// icebath does not know, tools where no event is about a tool, a pattern that can't be read, or a time limit out of bounds
func checkWebhook(raw any) error {
	entry, ok := raw.(map[string]any)
	if !ok {
		return &Refusal{Message: "A webhook is a set of named values, such as {url: https://example.com/hooks, events: [post_tool_call]}."}
	}
	for field, value := range entry {
		if !slices.Contains(webhookFields, field) {
			return &Refusal{Message: "A webhook has no setting called " + field + "."}
		}
		if _, text := value.(string); !text && value != nil && slices.Contains([]string{shellconfig.URL, shellconfig.Name, shellconfig.Secret,
			shellconfig.SecretEnv}, field) {
			return &Refusal{Message: "The " + field + " of a webhook is text."}
		}
	}
	address, _ := entry[shellconfig.URL].(string)
	lower := strings.ToLower(strings.TrimSpace(address))
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") || len(lower) <= len("https://") {
		return &Refusal{Message: "A webhook needs a web address that starts with https:// or http://, such as https://example.com/hooks."}
	}
	var events []string
	for _, item := range items(entry[shellconfig.Posted]) {
		name, _ := item.(string)
		if _, known := hooks.PointOf(name); !known {
			return &Refusal{Message: "icebath has no event “" + fmt.Sprint(item) + "” to post."}
		}
		events = append(events, name)
	}
	if len(events) == 0 {
		return &Refusal{Message: "A webhook needs at least one event to post."}
	}
	if err := checkMatcher(entry[shellconfig.Matcher], toolScoped(events), "Only a webhook that posts "+title(hooks.PreToolCall)+" or "+
		title(hooks.PostToolCall)+" can name tools."); err != nil {
		return err
	}
	return checkTimeout(entry[shellconfig.Timeout], MaxPostTimeout, "A post")
}

// Key of a webhook and the field inside it of a key under the outbound setting, both empty for the whole setting
func webhookAt(key string) (name, field string) {
	rest := strings.TrimPrefix(strings.TrimPrefix(key, shellconfig.Outbound), ".")
	name, field, _ = strings.Cut(rest, ".")
	return name, field
}

// Why a write of a key under the outbound setting would be refused, nil when it would be kept
func (r *Registrant) checkOutbound(cfg *config.Config, key string, value any) error {
	name, field := webhookAt(key)
	switch {
	case field != "":
		entry := webhookEntry(cfg, name)
		if entry == nil {
			return &Refusal{Message: "There is no webhook " + name + "."}
		}
		if value == nil {
			delete(entry, field)
		} else {
			entry[field] = value
		}
		return checkWebhook(entry)
	case name != "":
		if value == nil {
			return nil
		}
		return checkWebhook(value)
	case value == nil:
		return nil
	}
	table, ok := value.(map[string]any)
	if !ok {
		return &Refusal{Message: "Webhooks are kept by name, each a set of named values."}
	}
	for _, entry := range table {
		if err := checkWebhook(entry); err != nil {
			return err
		}
	}
	return nil
}

// Makes a write a person asked for of a key under the outbound setting through the function of what it changes
func (r *Registrant) writeOutbound(cfg *config.Config, key string, value any) (bool, error) {
	name, field := webhookAt(key)
	switch {
	case field != "":
		_, err := r.EditWebhook(cfg, name, field, value)
		return true, err
	case name != "" && value == nil:
		_, err := r.RemoveWebhook(cfg, name)
		return true, err
	}
	if err := r.checkOutbound(cfg, key, value); err != nil {
		return true, err
	}
	return true, cfg.Set(key, value)
}

// Takes every write a person asks for of the hooks and the webhooks, so the page, the commands and the settings service end in the same
// functions and refuse the same values
func (r *Registrant) Guard(cfg *config.Config) {
	cfg.Guard(shellconfig.Events, config.Guard{Check: r.checkEvents, Write: r.writeEvents})
	cfg.Guard(shellconfig.Outbound, config.Guard{Check: r.checkOutbound, Write: r.writeOutbound})
}
