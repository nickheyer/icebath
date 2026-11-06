package gatewaycli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

// WebhookArgs is one parsed icebath webhook command line, RouteProfile nil when the flag was not given
type WebhookArgs struct {
	Action, Name, Prompt, Events, Description, Skills, Deliver, DeliverChatID string
	Secret, Payload, Script, CronJob                                          string
	RouteProfile                                                              *string
	DeliverOnly, MirrorToSession                                              bool
}

type webhookAction func(ctx context.Context, w io.Writer, h string, args WebhookArgs) (int, error)

var (
	webhookNameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	webhookEnabled       = webhooksubs.WebhookEnabled
	webhookSubscriptions = webhooksubs.Load
	webhookTokenURLSafe  = tokenURLSafe
	webhookNow           = time.Now
	webhookCronJobRef    = cron.ResolveJobRef
	webhookActions       map[string]webhookAction
	errNoSubscription    = errors.New("no such subscription")
	webhookHTTPClient    = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.Response != nil && (req.Response.StatusCode == 307 || req.Response.StatusCode == 308) {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}}
)

func init() {
	webhookActions = map[string]webhookAction{
		"subscribe": webhookSubscribe, "add": webhookSubscribe,
		"list": webhookList, "ls": webhookList,
		"remove": webhookRemove, "rm": webhookRemove,
		"test": webhookTest}
}

// URL safe base64 of n random bytes without padding
func tokenURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Setup instructions printed when the webhook platform is disabled
func webhookSetupHint(h string) string {
	dhh := home.Display(h)
	return `
  Webhook platform is not enabled. To set it up:

  1. Run the gateway setup wizard:
     icebath gateway setup

  2. Or manually add to ` + dhh + `/config.yaml:
     platforms:
       webhook:
         enabled: true
         extra:
           port: 8644
           secret: "your-global-hmac-secret"

  3. Or set environment variables in ` + dhh + `/.env:
     WEBHOOK_ENABLED=true
     WEBHOOK_PORT=8644
     WEBHOOK_SECRET=your-global-secret

  Then start the gateway: icebath gateway run
`
}

// WebhookCommand runs icebath webhook and returns the exit status, 1 when a refusal wrote nothing
func WebhookCommand(ctx context.Context, w io.Writer, args WebhookArgs) (int, error) {
	h := home.Dir(ctx)
	if args.Action == "" {
		fmt.Fprintln(w, "Usage: icebath webhook {subscribe|list|remove|test}")
		fmt.Fprintln(w, "Run 'icebath webhook --help' for details.")
		return 0, nil
	}
	if !webhookEnabled(ctx, h) {
		fmt.Fprintln(w, webhookSetupHint(h))
		return 1, nil
	}
	if handler := webhookActions[args.Action]; handler != nil {
		return handler(ctx, w, h, args)
	}
	return 0, nil
}

func objGet(o *pyjson.Object, key string, def any) any {
	if v, ok := o.Get(key); ok {
		return v
	}
	return def
}

func splitStripped(s string) []any {
	out := []any{}
	for _, part := range strings.Split(s, ",") {
		out = append(out, pystr.Strip(part))
	}
	return out
}

// Validates and writes one route, refusing with status 1 before anything is written
func webhookSubscribe(ctx context.Context, w io.Writer, h string, args WebhookArgs) (int, error) {
	name := strings.ReplaceAll(pystr.Lower(pystr.Strip(args.Name)), " ", "-")
	if !webhookNameRe.MatchString(name) {
		fmt.Fprintf(w, "Error: Invalid name '%s'. Use lowercase alphanumeric with hyphens/underscores.\n", name)
		return 1, nil
	}
	subs := webhookSubscriptions(h)
	expected, isUpdate := subs.Get(name)
	if !isUpdate {
		expected = webhooksubs.Missing
	}
	existing, ok := expected.(*pyjson.Object)
	if !ok {
		existing = pyjson.NewObject()
	}
	var profile any
	if args.RouteProfile == nil {
		profile = objGet(existing, "profile", "default")
	} else {
		normalized, err := profiles.NormalizeProfileName(*args.RouteProfile)
		if err == nil {
			err = profiles.ValidateProfileName(normalized)
		}
		if err != nil {
			fmt.Fprintf(w, "Error: %s\n", err.Error())
			return 1, nil
		}
		if !profiles.ProfileExists(normalized) {
			fmt.Fprintf(w, "Error: Profile '%s' does not exist.\n", normalized)
			return 1, nil
		}
		profile = normalized
	}
	profileChanged := isUpdate && !pyrepr.Equal(profile, objGet(existing, "profile", "default"))
	if profileChanged && args.Secret != "" && pyrepr.Equal(args.Secret, objGet(existing, "secret", nil)) {
		fmt.Fprintln(w, "Error: Rebinding a webhook route requires a new HMAC secret. "+
			"Omit --secret to generate one automatically, or provide a different secret.")
		return 1, nil
	}
	var secret any = args.Secret
	if args.Secret == "" {
		secret = nil
		if !profileChanged {
			secret = objGet(existing, "secret", nil)
		}
		if !pyrepr.Truthy(secret) {
			secret = webhookTokenURLSafe(32)
		}
	}
	events := []any{}
	if args.Events != "" {
		events = splitStripped(args.Events)
	}
	skills := []any{}
	if args.Skills != "" {
		skills = splitStripped(args.Skills)
	}
	description := args.Description
	if description == "" {
		description = "Agent-created subscription: " + name
	}
	deliver := args.Deliver
	if deliver == "" {
		deliver = "log"
	}
	route := pyjson.NewObject()
	route.Set("description", description)
	route.Set("events", events)
	route.Set("secret", secret)
	route.Set("prompt", args.Prompt)
	route.Set("skills", skills)
	route.Set("deliver", deliver)
	route.Set("profile", profile)
	route.Set("created_at", webhookNow().UTC().Format("2006-01-02T15:04:05Z"))
	if args.DeliverOnly {
		if args.CronJob != "" {
			fmt.Fprintln(w, "Error: --deliver-only and --cron-job are mutually exclusive. "+
				"--deliver-only pushes the rendered template as a message; "+
				"--cron-job fires an existing cron job (which handles its own "+
				"delivery).")
			return 1, nil
		}
		if deliver == "log" {
			fmt.Fprintln(w, "Error: --deliver-only requires --deliver to be a real target "+
				"(telegram, discord, slack, github_comment, etc.) — not 'log'.")
			return 1, nil
		}
		route.Set("deliver_only", true)
	}
	if args.MirrorToSession {
		route.Set("mirror_to_session", true)
	}
	if cronJob := pystr.Strip(args.CronJob); cronJob != "" {
		job, err := resolveRouteCronJob(h, profile, cronJob)
		var ambiguous *cron.AmbiguousJobReferenceError
		if errors.As(err, &ambiguous) {
			fmt.Fprintf(w, "Error: %s\n", err.Error())
			return 1, nil
		}
		if err != nil {
			return 1, err
		}
		if job == nil {
			fmt.Fprintf(w, "Error: no cron job matches '%s'. List jobs with: icebath cron list\n", cronJob)
			return 1, nil
		}
		route.Set("cron_job", job.ID)
	}
	if script := pystr.Strip(args.Script); script != "" {
		route.Set("script", script)
	}
	if args.DeliverChatID != "" {
		extra := pyjson.NewObject()
		extra.Set("chat_id", args.DeliverChatID)
		route.Set("deliver_extra", extra)
	}
	written, err := webhooksubs.Replace(h, name, route, expected)
	var conflict *webhooksubs.SubscriptionMutationConflict
	if errors.As(err, &conflict) {
		fmt.Fprintf(w, "Error: %s\n", err.Error())
		return 1, nil
	}
	if err != nil {
		return 1, err
	}
	verb := "Created"
	if isUpdate {
		verb = "Updated"
	}
	fmt.Fprintf(w, "\n  %s webhook subscription: %s\n", verb, name)
	fmt.Fprintf(w, "  URL:    %s\n", webhooksubs.RouteURL(ctx, h, name, written))
	fmt.Fprintf(w, "  Profile: %s\n", pyrepr.Str(profile))
	fmt.Fprintf(w, "  Secret: %s\n", pyrepr.Str(secret))
	shown := make([]string, len(events))
	for i, e := range events {
		shown[i] = e.(string)
	}
	allEvents := strings.Join(shown, ", ")
	if allEvents == "" {
		allEvents = "(all)"
	}
	fmt.Fprintf(w, "  Events: %s\n", allEvents)
	fmt.Fprintf(w, "  Deliver: %s\n", pyrepr.Str(objGet(written, "deliver", nil)))
	if pyrepr.Truthy(objGet(written, "deliver_only", nil)) {
		fmt.Fprintln(w, "  Mode: direct delivery (no agent, zero LLM cost)")
	}
	if pyrepr.Truthy(objGet(written, "mirror_to_session", nil)) {
		fmt.Fprintln(w, "  Replies: each delivery is mirrored into the target chat's session")
	}
	if v := objGet(written, "cron_job", nil); pyrepr.Truthy(v) {
		fmt.Fprintf(w, "  Mode: cron-job trigger — fires job '%s' on each event\n", pyrepr.Str(v))
	}
	if v := objGet(written, "prompt", nil); pyrepr.Truthy(v) {
		runes := []rune(pyrepr.Str(v))
		preview := string(runes[:min(80, len(runes))])
		if len(runes) > 80 {
			preview += "..."
		}
		label := "Prompt"
		if pyrepr.Truthy(objGet(written, "deliver_only", nil)) {
			label = "Message"
		}
		fmt.Fprintf(w, "  %s: %s\n", label, preview)
	}
	if v := objGet(written, "script", nil); pyrepr.Truthy(v) {
		fmt.Fprintf(w, "  Script: %s\n", pyrepr.Str(v))
	}
	fmt.Fprintln(w, "\n  Configure your service to POST to the URL above.")
	fmt.Fprintln(w, "  Use the secret for HMAC-SHA256 signature validation.")
	fmt.Fprintln(w, "  The gateway must be running to receive events (icebath gateway run).")
	fmt.Fprintln(w)
	return 0, nil
}

// Resolves a cron reference in the route profile store, the caller home for the default profile
func resolveRouteCronJob(h string, profile any, reference string) (*cron.Job, error) {
	if pyrepr.Equal(profile, "default") {
		return webhookCronJobRef(h, reference)
	}
	dir, err := profiles.GetProfileDir(pyrepr.Str(profile))
	if err != nil {
		return nil, err
	}
	return webhookCronJobRef(dir, reference)
}

// Route as a dict or the AttributeError a get on another type raises
func routeObject(route any) (*pyjson.Object, error) {
	if o, ok := route.(*pyjson.Object); ok {
		return o, nil
	}
	return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(route))
}

// Comma joined event names or the TypeError str.join raises on a non string item
func joinEvents(v any) (string, error) {
	items, ok := v.([]any)
	if !ok {
		if s, isStr := v.(string); isStr {
			return strings.Join(strings.Split(s, ""), ", "), nil
		}
		if o, isObj := v.(*pyjson.Object); isObj {
			keys := make([]any, len(o.Keys))
			for i, k := range o.Keys {
				keys[i] = k
			}
			return joinEvents(keys)
		}
		return "", fmt.Errorf("can only join an iterable")
	}
	parts := make([]string, len(items))
	for i, item := range items {
		s, isStr := item.(string)
		if !isStr {
			return "", fmt.Errorf("sequence item %d: expected str instance, %s found", i, pyrepr.TypeName(item))
		}
		parts[i] = s
	}
	return strings.Join(parts, ", "), nil
}

// Prints every dynamic subscription with its URL, profile, events and delivery
func webhookList(ctx context.Context, w io.Writer, h string, _ WebhookArgs) (int, error) {
	subs := webhookSubscriptions(h)
	if subs.Len() == 0 {
		fmt.Fprintln(w, "  No dynamic webhook subscriptions.")
		fmt.Fprintln(w, "  Create one with: icebath webhook subscribe <name>")
		return 0, nil
	}
	fmt.Fprintf(w, "\n  %d webhook subscription(s):\n\n", subs.Len())
	for _, name := range subs.Keys {
		route, err := routeObject(subs.Vals[name])
		if err != nil {
			return 1, err
		}
		events, err := joinEvents(objGet(route, "events", []any{}))
		if err != nil {
			return 1, err
		}
		if events == "" {
			events = "(all)"
		}
		deliver := pyrepr.Str(objGet(route, "deliver", "log"))
		if pyrepr.Truthy(objGet(route, "deliver_only", nil)) {
			deliver += " (direct — no agent)"
		}
		if v := objGet(route, "cron_job", nil); pyrepr.Truthy(v) {
			deliver = fmt.Sprintf("cron job '%s'", pyrepr.Str(v))
		}
		fmt.Fprintf(w, "  ◆ %s\n", name)
		if desc := objGet(route, "description", ""); pyrepr.Truthy(desc) {
			fmt.Fprintf(w, "    %s\n", pyrepr.Str(desc))
		}
		fmt.Fprintf(w, "    URL:     %s\n", webhooksubs.RouteURL(ctx, h, name, route))
		fmt.Fprintf(w, "    Profile: %s\n", pyrepr.Str(objGet(route, "profile", "default")))
		fmt.Fprintf(w, "    Events:  %s\n", events)
		fmt.Fprintf(w, "    Deliver: %s\n", deliver)
		if v := objGet(route, "script", nil); pyrepr.Truthy(v) {
			fmt.Fprintf(w, "    Script:  %s\n", pyrepr.Str(v))
		}
		fmt.Fprintln(w)
	}
	return 0, nil
}

// Removes one dynamic subscription under the store lock
func webhookRemove(_ context.Context, w io.Writer, h string, args WebhookArgs) (int, error) {
	name := pystr.Lower(pystr.Strip(args.Name))
	err := webhooksubs.Mutate(h, func(subs *pyjson.Object) error {
		if _, ok := subs.Get(name); !ok {
			return errNoSubscription
		}
		subs.Delete(name)
		return nil
	})
	if errors.Is(err, errNoSubscription) {
		fmt.Fprintf(w, "  No subscription named '%s'.\n", name)
		fmt.Fprintln(w, "  Note: Static routes from config.yaml cannot be removed here.")
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(w, "  Removed webhook subscription: %s\n", name)
	return 0, nil
}

// Posts a signed test payload to a route and prints the response or the failure
func webhookTest(ctx context.Context, w io.Writer, h string, args WebhookArgs) (int, error) {
	name := pystr.Lower(pystr.Strip(args.Name))
	subs := webhookSubscriptions(h)
	raw, ok := subs.Get(name)
	if !ok {
		fmt.Fprintf(w, "  No subscription named '%s'.\n", name)
		return 0, nil
	}
	route, err := routeObject(raw)
	if err != nil {
		return 1, err
	}
	secret, isStr := objGet(route, "secret", "").(string)
	if !isStr {
		return 1, fmt.Errorf("'%s' object has no attribute 'encode'", pyrepr.TypeName(objGet(route, "secret", "")))
	}
	url := webhooksubs.RouteURL(ctx, h, name, route)
	payload := args.Payload
	if payload == "" {
		payload = `{"test": true, "event_type": "test", "message": "Hello from icebath webhook test"}`
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	fmt.Fprintf(w, "  Sending test POST to %s\n", url)
	status, body, err := postWebhookTest(ctx, url, payload, sig)
	if err != nil {
		fmt.Fprintf(w, "  Error: %s\n", err.Error())
		fmt.Fprintln(w, "  Is the gateway running? (icebath gateway run)")
		return 0, nil
	}
	fmt.Fprintf(w, "  Response (%d): %s\n", status, body)
	return 0, nil
}

// One urllib style POST, a non 2xx final status failing as HTTPError and transport failures as URLError text
func postWebhookTest(ctx context.Context, url, payload, sig string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return 0, "", fmt.Errorf("<urlopen error %s>", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "test")
	req.Header.Set("User-Agent", "icebath-agent")
	resp, err := webhookHTTPClient.Do(req)
	if err != nil {
		return 0, "", urlopenError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" "))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		if isTimeout(err) {
			return 0, "", errors.New("timed out")
		}
		return 0, "", err
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		return 0, "", err
	}
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		text = "\ufeff" + text
	}
	return resp.StatusCode, text, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// URLError text of a transport failure, errno and strerror for socket errors
func urlopenError(err error) error {
	if isTimeout(err) {
		return errors.New("<urlopen error timed out>")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return errors.New("<urlopen error [Errno -2] Name or service not known>")
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("<urlopen error [Errno %d] %s>", int(errno), pystr.Capitalize(errno.Error()))
	}
	return fmt.Errorf("<urlopen error %s>", err.Error())
}
