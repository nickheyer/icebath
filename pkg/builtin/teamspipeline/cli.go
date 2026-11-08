package teamspipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	cliOut            io.Writer = os.Stdout
	loadGatewayConfig           = gateway.LoadGatewayConfig
	cliGraphClient              = buildGraphClient
	// Graph token provider over the MSGRAPH process environment, swapped by tests
	graphTokenProviderFromEnv = func() (*messagetools.MicrosoftGraphTokenProvider, error) {
		return messagetools.MicrosoftGraphTokenProviderFromEnv(nil)
	}
)

// One argparse argument of a subcommand, positional when the flag has no leading dash
type cliOpt struct {
	flag     string
	kind     byte
	def      any
	required bool
	help     string
}

// Subcommand row with its arguments, handler and the up front non blank argument check
type cliSubcommand struct {
	name     string
	aliases  []string
	help     string
	options  []cliOpt
	handler  func(ctx context.Context, ns map[string]any) error
	required []string
	missing  string
}

var (
	storePathOpt     = cliOpt{flag: "--store-path", kind: 's', def: ""}
	teamsSubcommands []cliSubcommand
)

// Builds the subcommand table in the order that defines both the help listing and the usage string
func init() {
	str := func(flag string) cliOpt { return cliOpt{flag: flag, kind: 's', def: ""} }
	teamsSubcommands = []cliSubcommand{
		{name: "list", aliases: []string{"ls"}, help: "List recent Teams pipeline jobs",
			options: []cliOpt{{flag: "--limit", kind: 'i', def: 20}, str("--status"), storePathOpt}, handler: cmdList},
		{name: "show", help: "Show a stored Teams pipeline job", options: []cliOpt{{flag: "job_id", kind: 's'}, storePathOpt},
			handler: cmdShow, required: []string{"job_id"}, missing: "job_id is required"},
		{name: "run", aliases: []string{"replay"}, help: "Replay a stored Teams pipeline job", options: []cliOpt{{flag: "job_id", kind: 's'}, storePathOpt},
			handler: cmdRun, required: []string{"job_id"}, missing: "job_id is required"},
		{name: "fetch", aliases: []string{"test"}, help: "Dry-run meeting artifact resolution",
			options: []cliOpt{str("--meeting-id"), str("--join-web-url"),
				{flag: "--organizer-user-id", kind: 's', def: "", help: "Microsoft Entra user ID for organizer-scoped online meeting lookup"},
				str("--tenant-id"), str("--call-record-id")}, handler: cmdFetch},
		{name: "subscriptions", aliases: []string{"subs"}, help: "List Graph subscriptions", options: []cliOpt{storePathOpt}, handler: cmdSubscriptions},
		{name: "subscribe", help: "Create a Microsoft Graph subscription",
			options: []cliOpt{{flag: "--resource", kind: 's', required: true}, {flag: "--notification-url", kind: 's', required: true},
				str("--change-type"), str("--expiration"), str("--client-state"), str("--lifecycle-notification-url"),
				{flag: "--latest-supported-tls-version", kind: 's', def: "v1_2"}, storePathOpt}, handler: cmdSubscribe},
		{name: "renew-subscription", help: "Renew a Microsoft Graph subscription",
			options: []cliOpt{{flag: "subscription_id", kind: 's'}, {flag: "--expiration", kind: 's', required: true}, storePathOpt},
			handler: cmdRenewSubscription, required: []string{"subscription_id", "expiration"}, missing: "subscription_id and --expiration are required"},
		{name: "delete-subscription", help: "Delete a Microsoft Graph subscription", options: []cliOpt{{flag: "subscription_id", kind: 's'}, storePathOpt},
			handler: cmdDeleteSubscription, required: []string{"subscription_id"}, missing: "subscription_id is required"},
		{name: "maintain-subscriptions", help: "Renew near-expiry managed subscriptions",
			options: []cliOpt{{flag: "--renew-within-hours", kind: 'i', def: 24}, {flag: "--extend-hours", kind: 'i', def: 24},
				{flag: "--dry-run", kind: 'b', def: false}, storePathOpt, str("--client-state")}, handler: cmdMaintainSubscriptions},
		{name: "token-health", aliases: []string{"token"}, help: "Inspect Graph token health",
			options: []cliOpt{{flag: "--force-refresh", kind: 'b', def: false}}, handler: cmdTokenHealth},
		{name: "validate", help: "Validate Teams pipeline configuration snapshot", options: []cliOpt{storePathOpt}, handler: cmdValidate},
	}
}

// Namespace attribute name argparse derives from a flag or positional
func (o cliOpt) dest() string {
	return strings.ReplaceAll(strings.TrimLeft(o.flag, "-"), "-", "_")
}

// Cobra command whose run collects the parsed arguments into a namespace for the dispatcher
func (s *cliSubcommand) cobraCommand() *cobra.Command {
	use := []string{s.name}
	positional := []cliOpt{}
	for _, o := range s.options {
		if !strings.HasPrefix(o.flag, "-") {
			positional = append(positional, o)
			use = append(use, o.flag)
		}
	}
	cmd := &cobra.Command{Use: strings.Join(use, " "), Aliases: s.aliases, Short: s.help, Args: cobra.ExactArgs(len(positional))}
	for _, o := range s.options {
		if !strings.HasPrefix(o.flag, "-") {
			continue
		}
		name := strings.TrimLeft(o.flag, "-")
		switch o.kind {
		case 'i':
			cmd.Flags().Int(name, o.def.(int), o.help)
		case 'b':
			cmd.Flags().Bool(name, false, o.help)
		default:
			def, _ := o.def.(string)
			cmd.Flags().String(name, def, o.help)
		}
		if o.required {
			_ = cmd.MarkFlagRequired(name)
		}
	}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		ns := map[string]any{"teams_pipeline_action": c.CalledAs()}
		for i, o := range positional {
			ns[o.dest()] = args[i]
		}
		for _, o := range s.options {
			if !strings.HasPrefix(o.flag, "-") {
				continue
			}
			name := strings.TrimLeft(o.flag, "-")
			switch o.kind {
			case 'i':
				ns[o.dest()], _ = c.Flags().GetInt(name)
			case 'b':
				ns[o.dest()], _ = c.Flags().GetBool(name)
			default:
				ns[o.dest()], _ = c.Flags().GetString(name)
			}
		}
		return teamsPipelineCommand(ns)
	}
	return cmd
}

// Adds every teams-pipeline subcommand and its aliases under the parser and defaults the run to the dispatcher
func registerCLI(parser any) {
	root, ok := parser.(*cobra.Command)
	if !ok {
		panic(fmt.Sprintf("AttributeError: '%s' object has no attribute 'add_subparsers'", pyrepr.TypeName(parser)))
	}
	for i := range teamsSubcommands {
		root.AddCommand(teamsSubcommands[i].cobraCommand())
	}
	root.RunE = func(*cobra.Command, []string) error {
		return teamsPipelineCommand(map[string]any{"teams_pipeline_action": nil})
	}
}

// Dispatches the parsed teams-pipeline namespace, a non zero exit code carried as an exit error
func teamsPipelineCommand(args any) error {
	ns, _ := args.(map[string]any)
	code, err := runTeamsPipeline(context.Background(), ns)
	if err != nil {
		return err
	}
	if code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}

// Runs the named action returning 2 for usage errors, 1 for missing Graph setup and 0 otherwise
func runTeamsPipeline(ctx context.Context, ns map[string]any) (int, error) {
	action := ns["teams_pipeline_action"]
	if !pyrepr.Truthy(action) {
		names := make([]string, len(teamsSubcommands))
		for i, s := range teamsSubcommands {
			names[i] = s.name
		}
		fmt.Fprintf(cliOut, "Usage: icebath teams-pipeline {%s}\n", strings.Join(names, "|"))
		return 2, nil
	}
	sub := lookupTeamsAction(action)
	if sub == nil {
		fmt.Fprintf(cliOut, "Unknown teams-pipeline action: %s\n", pyrepr.Str(action))
		return 2, nil
	}
	for _, name := range sub.required {
		if textArg(ns, name) == "" {
			fmt.Fprintln(cliOut, sub.missing)
			return 0, nil
		}
	}
	if err := sub.handler(ctx, ns); err != nil {
		var cfgErr *messagetools.MicrosoftGraphConfigError
		if errors.As(err, &cfgErr) {
			fmt.Fprintln(cliOut, graphSetupHint(ctx))
			return 1, nil
		}
		return 0, err
	}
	return 0, nil
}

// Subcommand whose name or alias equals the action, nil when none does
func lookupTeamsAction(action any) *cliSubcommand {
	name, ok := action.(string)
	if !ok {
		return nil
	}
	for i, s := range teamsSubcommands {
		if s.name == name {
			return &teamsSubcommands[i]
		}
		for _, a := range s.aliases {
			if a == name {
				return &teamsSubcommands[i]
			}
		}
	}
	return nil
}

// Stripped string form of an optional argument, empty when absent or falsy
func textArg(ns map[string]any, name string) string {
	v := ns[name]
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Integer argument with the default replacing an absent or falsy value
func intArg(ns map[string]any, name string, def int) (int, error) {
	v, ok := ns[name]
	if !ok || !pyrepr.Truthy(v) {
		return def, nil
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case *big.Int:
		return int(n.Int64()), nil
	case float64:
		return int(n), nil
	case bool:
		return 1, nil
	case string:
		i, err := strconv.Atoi(strings.ReplaceAll(pystr.Strip(n), "_", ""))
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.StrRepr(n))
		}
		return i, nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))
}

// Store at the store_path argument or the resolved default path
func openStore(ctx context.Context, ns map[string]any) (*TeamsPipelineStore, error) {
	path := ""
	if v := ns["store_path"]; v != nil {
		path = pyrepr.Str(v)
	}
	return NewTeamsPipelineStore(resolveTeamsPipelineStorePath(ctx, path))
}

// Prints the payload as sorted two space indented JSON
func printJSON(payload any) error {
	s, err := pyjson.Dumps(payload, pyjson.Indent(2), pyjson.SortKeys(true))
	if err != nil {
		return err
	}
	fmt.Fprintln(cliOut, s)
	return nil
}

// Labelled value of a record line, printed when required or truthy
type recordField struct {
	label    string
	value    any
	required bool
}

// Bulleted record with its title and fields
type cliRecord struct {
	title  any
	fields []recordField
}

// Prints bulleted records under a count heading, or the empty message when there are none
func printRecords(noun, emptyMessage string, records []cliRecord) {
	if len(records) == 0 {
		fmt.Fprintln(cliOut, emptyMessage)
		return
	}
	fmt.Fprintf(cliOut, "\n%d %s:\n\n", len(records), noun)
	for _, r := range records {
		fmt.Fprintf(cliOut, "  \u25c6 %s\n", pyrepr.Str(r.title))
		for _, f := range r.fields {
			if f.required || pyrepr.Truthy(f.value) {
				fmt.Fprintf(cliOut, "    %s: %s\n", f.label, pyrepr.Str(f.value))
			}
		}
		fmt.Fprintln(cliOut)
	}
}

// Setup hint naming the MSGRAPH variables to add to the home env file
func graphSetupHint(ctx context.Context) string {
	return fmt.Sprintf(`
  Microsoft Graph is not configured. Add these to %s/.env:

    MSGRAPH_TENANT_ID=...
    MSGRAPH_CLIENT_ID=...
    MSGRAPH_CLIENT_SECRET=...

  Then restart the gateway or rerun this command.
`, home.Display(home.Dir(ctx)))
}

// Graph collection resources that only emit created notifications
var createdResourcePrefixes = []string{
	"communications/onlinemeetings/getalltranscripts", "communications/onlinemeetings/getallrecordings", "communications/callrecords"}

// Created for transcript, recording and call record collections, updated for everything else
func defaultChangeTypeForResource(resource string) string {
	r := pystr.Lower(pystr.Strip(resource))
	for _, p := range createdResourcePrefixes {
		if strings.HasPrefix(r, p) {
			return "created"
		}
	}
	return "updated"
}

// Job copy whose summary transcript is cut to a 240 character preview, an empty summary becoming null
func compactJob(job *pyjson.Object) (*pyjson.Object, error) {
	payload := job.Copy()
	summary := pyjson.NewObject()
	if raw := get(payload, "summary_payload"); pyrepr.Truthy(raw) {
		m, err := mapping(raw)
		if err != nil {
			return nil, err
		}
		summary = m.Copy()
	}
	if transcript, _ := summary.Pop("transcript_text"); pyrepr.Truthy(transcript) {
		summary.Set("transcript_preview", pystr.Slice(pyrepr.Str(transcript), 0, 240))
	}
	if summary.Len() > 0 {
		payload.Set("summary_payload", summary)
	} else {
		payload.Set("summary_payload", nil)
	}
	return payload, nil
}

// Ordered dict view of a JSON mapping value, a type error for anything else
func mapping(v any) (*pyjson.Object, error) {
	switch m := v.(type) {
	case *pyjson.Object:
		return m, nil
	case map[string]any:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := pyjson.NewObject()
		for _, k := range keys {
			o.Set(k, m[k])
		}
		return o, nil
	}
	return nil, fmt.Errorf("'%s' object is not a mapping", pyrepr.TypeName(v))
}

// Value under key of a mapping, the AttributeError of get on anything else
func attrGet(v any, key string) (any, error) {
	m, err := mapping(v)
	if err != nil {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
	}
	return get(m, key), nil
}

// First truthy value or the last one
func orValue(vals ...any) any {
	for _, v := range vals[:len(vals)-1] {
		if pyrepr.Truthy(v) {
			return v
		}
	}
	return vals[len(vals)-1]
}

// Prints the Graph credential, webhook, Teams delivery and store readiness snapshot
func cmdValidate(ctx context.Context, ns map[string]any) error {
	store, err := openStore(ctx, ns)
	if err != nil {
		return err
	}
	issues, warnings := []any{}, []any{}
	gc, err := loadGatewayConfig(ctx)
	if err != nil {
		return err
	}
	webhook := gc.Platforms[gateway.PlatformMSGraphWebhook]
	teams := gc.Platforms[gateway.Platform("teams")]
	graph := pyjson.NewObject()
	graphComplete := true
	for _, key := range []string{"tenant_id", "client_id", "client_secret"} {
		set := os.Getenv("MSGRAPH_"+strings.ToUpper(key)) != ""
		graph.Set(key, set)
		graphComplete = graphComplete && set
	}
	webhookEnabled := webhook != nil && webhook.Enabled
	teamsEnabled := teams != nil && teams.Enabled
	teamsExtra := map[string]any{}
	if teams != nil {
		for k, v := range teams.Extra {
			teamsExtra[k] = v
		}
	}
	var teamsMode any
	if mode := pystr.Strip(pyrepr.Str(orValue(teamsExtra["delivery_mode"], ""))); mode != "" {
		teamsMode = mode
	}
	if !graphComplete {
		issues = append(issues, "Microsoft Graph app-only credentials are incomplete.")
	}
	if !webhookEnabled {
		issues = append(issues, "MSGRAPH_WEBHOOK_ENABLED is not enabled.")
	}
	switch {
	case !teamsEnabled:
		warnings = append(warnings, "Teams outbound delivery is disabled.")
	case teamsMode == "incoming_webhook":
		if !pyrepr.Truthy(teamsExtra["incoming_webhook_url"]) {
			issues = append(issues, "TEAMS_INCOMING_WEBHOOK_URL is required for incoming_webhook mode.")
		}
	case teamsMode == "graph":
		if !pyrepr.Truthy(teams.Token) && !pyrepr.Truthy(teamsExtra["access_token"]) && !graphComplete {
			issues = append(issues, "TEAMS_GRAPH_ACCESS_TOKEN or complete MSGRAPH_* app credentials is required for graph delivery mode.")
		}
		if !pyrepr.Truthy(teamsExtra["team_id"]) {
			issues = append(issues, "TEAMS_TEAM_ID is required for graph delivery mode.")
		}
		if !pyrepr.Truthy(teamsExtra["channel_id"]) && !pyrepr.Truthy(teamsExtra["chat_id"]) && teams.HomeChannel == nil {
			issues = append(issues, "TEAMS_CHANNEL_ID is required for graph delivery mode.")
		}
	default:
		warnings = append(warnings, "TEAMS_DELIVERY_MODE is not set.")
	}
	return printJSON(obj("ok", len(issues) == 0, "issues", issues, "warnings", warnings, "graph_config", graph,
		"webhook_enabled", webhookEnabled, "teams_enabled", teamsEnabled, "teams_delivery_mode", teamsMode,
		"store_path", store.Path, "store_stats", store.Stats()))
}

// Prints the most recently updated jobs, filtered by status and capped by limit
func cmdList(ctx context.Context, ns map[string]any) error {
	store, err := openStore(ctx, ns)
	if err != nil {
		return err
	}
	all := store.ListJobs()
	jobs := make([]any, 0, all.Len())
	for _, k := range all.Keys {
		jobs = append(jobs, all.Vals[k])
	}
	if status := pystr.Lower(textArg(ns, "status")); status != "" {
		kept := jobs[:0:0]
		for _, job := range jobs {
			s, err := attrGet(job, "status")
			if err != nil {
				return err
			}
			if pystr.Lower(pyrepr.Str(orValue(s, ""))) == status {
				kept = append(kept, job)
			}
		}
		jobs = kept
	}
	keys := make([]string, len(jobs))
	for i, job := range jobs {
		u, err := attrGet(orValue(job, pyjson.NewObject()), "updated_at")
		if err != nil {
			return err
		}
		keys[i] = pyrepr.Str(orValue(u, ""))
	}
	idx := make([]int, len(jobs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return keys[idx[a]] > keys[idx[b]] })
	limit, err := intArg(ns, "limit", 20)
	if err != nil {
		return err
	}
	n := max(1, min(limit, 100))
	records := []cliRecord{}
	for _, i := range idx {
		if len(records) >= n {
			break
		}
		job, err := mapping(jobs[i])
		if err != nil {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(jobs[i]))
		}
		ref, err := attrGet(orValue(get(job, "meeting_ref"), pyjson.NewObject()), "meeting_id")
		if err != nil {
			return err
		}
		records = append(records, cliRecord{title: get(job, "job_id"), fields: []recordField{
			{"status", get(job, "status"), true},
			{"meeting", orValue(ref, "unknown"), true},
			{"strategy", get(job, "selected_artifact_strategy"), false},
			{"updated", get(job, "updated_at"), false},
			{"error", get(job, "error_info"), false}}})
	}
	printRecords("Teams pipeline job(s)", "No Teams meeting pipeline jobs found.", records)
	return nil
}

// Prints one stored job in compact form, or the unknown job line
func cmdShow(ctx context.Context, ns map[string]any) error {
	jobID := textArg(ns, "job_id")
	store, err := openStore(ctx, ns)
	if err != nil {
		return err
	}
	job := store.GetJob(jobID)
	if job == nil || job.Len() == 0 {
		fmt.Fprintf(cliOut, "Unknown job: %s\n", jobID)
		return nil
	}
	compact, err := compactJob(job)
	if err != nil {
		return err
	}
	return printJSON(compact)
}

// Replays a stored job through the pipeline and prints the compact result
func cmdRun(ctx context.Context, ns map[string]any) error {
	client, err := cliGraphClient()
	if err != nil {
		return err
	}
	store, err := openStore(ctx, ns)
	if err != nil {
		return err
	}
	pipeline, err := NewTeamsMeetingPipeline(client, store, map[string]any{})
	if err != nil {
		return err
	}
	job, err := pipeline.RunJob(ctx, textArg(ns, "job_id"))
	if err != nil {
		return err
	}
	compact, err := compactJob(job.ToDict())
	if err != nil {
		return err
	}
	return printJSON(compact)
}

// Resolves a meeting and prints its transcript, recordings and call record without running the pipeline
func cmdFetch(ctx context.Context, ns map[string]any) error {
	meetingID, joinWebURL := textArg(ns, "meeting_id"), textArg(ns, "join_web_url")
	if meetingID == "" && joinWebURL == "" {
		fmt.Fprintln(cliOut, "meeting_id or join_web_url is required")
		return nil
	}
	client, err := cliGraphClient()
	if err != nil {
		return err
	}
	ref, err := resolveMeetingReference(ctx, client, meetingID, joinWebURL, textArg(ns, "tenant_id"), textArg(ns, "organizer_user_id"))
	if err != nil {
		return err
	}
	artifact, text, err := fetchPreferredTranscriptText(ctx, client, ref)
	if err != nil {
		return err
	}
	recordings, err := listRecordingArtifacts(ctx, client, ref)
	if err != nil {
		return err
	}
	callRecord, err := enrichMeetingWithCallRecord(ctx, client, ref, textArg(ns, "call_record_id"), true)
	if err != nil {
		return err
	}
	var artifactDict, preview, callDict any
	if artifact != nil {
		artifactDict = artifact.ToDict()
	}
	if p := pystr.Slice(text, 0, 240); p != "" {
		preview = p
	}
	if callRecord != nil {
		callDict = callRecord.ToDict()
	}
	recs := []any{}
	for i, r := range recordings {
		if i == 5 {
			break
		}
		recs = append(recs, r.ToDict())
	}
	return printJSON(obj("meeting_ref", ref.ToDict(), "transcript_available", artifact != nil && text != "",
		"transcript_artifact", artifactDict, "transcript_preview", preview, "recording_count", len(recordings),
		"recordings", recs, "call_record", callDict))
}

// Syncs every remote Graph subscription into the store and prints them
func cmdSubscriptions(ctx context.Context, ns map[string]any) error {
	store, err := openStore(ctx, ns)
	if err != nil {
		return err
	}
	client, err := cliGraphClient()
	if err != nil {
		return err
	}
	subs, err := client.CollectPaginated(ctx, "/subscriptions", nil, nil)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if m, err := mapping(sub); err == nil {
			_, _ = syncGraphSubscriptionRecord(store, m, "active", false)
		}
	}
	records := []cliRecord{}
	for _, sub := range subs {
		vals := map[string]any{}
		for _, k := range []string{"id", "resource", "changeType", "expirationDateTime", "notificationUrl"} {
			v, err := attrGet(sub, k)
			if err != nil {
				return err
			}
			vals[k] = v
		}
		records = append(records, cliRecord{title: orValue(vals["id"], "unknown"), fields: []recordField{
			{"resource", orValue(vals["resource"], "unknown"), true},
			{"changeType", orValue(vals["changeType"], "unknown"), true},
			{"expires", vals["expirationDateTime"], false},
			{"notify", vals["notificationUrl"], false}}})
	}
	printRecords("Microsoft Graph subscription(s)", "No Microsoft Graph subscriptions found.", records)
	return nil
}

// Creates a Graph subscription, records it as active and prints the Graph response
func cmdSubscribe(ctx context.Context, ns map[string]any) error {
	store, err := openStore(ctx, ns)
	if err != nil {
		return err
	}
	resource := textArg(ns, "resource")
	changeType := textArg(ns, "change_type")
	if changeType == "" {
		changeType = defaultChangeTypeForResource(resource)
	}
	expiration := textArg(ns, "expiration")
	if expiration == "" {
		expiration = utcTimestamp(1, nil)
	}
	tls := textArg(ns, "latest_supported_tls_version")
	if tls == "" {
		tls = "v1_2"
	}
	payload := obj("changeType", changeType, "notificationUrl", textArg(ns, "notification_url"), "resource", resource,
		"expirationDateTime", expiration, "latestSupportedTlsVersion", tls)
	if cs := textArg(ns, "client_state"); cs != "" {
		payload.Set("clientState", cs)
	}
	if lc := textArg(ns, "lifecycle_notification_url"); lc != "" {
		payload.Set("lifecycleNotificationUrl", lc)
	}
	client, err := cliGraphClient()
	if err != nil {
		return err
	}
	result, err := client.PostJSON(ctx, "/subscriptions", payload, nil)
	if err != nil {
		return err
	}
	m, err := mapping(result)
	if err != nil {
		return err
	}
	if _, err := syncGraphSubscriptionRecord(store, m, "active", false); err != nil {
		return err
	}
	return printJSON(result)
}

// Extends a Graph subscription expiry, records the renewal and prints the merged record
func cmdRenewSubscription(ctx context.Context, ns map[string]any) error {
	subscriptionID, expiration := textArg(ns, "subscription_id"), textArg(ns, "expiration")
	store, err := openStore(ctx, ns)
	if err != nil {
		return err
	}
	client, err := cliGraphClient()
	if err != nil {
		return err
	}
	result, err := client.PatchJSON(ctx, "/subscriptions/"+subscriptionID, obj("expirationDateTime", expiration), nil)
	if err != nil {
		return err
	}
	merged := obj("id", subscriptionID)
	if pyrepr.Truthy(result) {
		m, err := mapping(result)
		if err != nil {
			return fmt.Errorf("'%s' object is not a mapping", pyrepr.TypeName(result))
		}
		for _, k := range m.Keys {
			merged.Set(k, m.Vals[k])
		}
	}
	merged.Set("expirationDateTime", expiration)
	if _, err := syncGraphSubscriptionRecord(store, merged, "active", true); err != nil {
		return err
	}
	return printJSON(merged)
}

// Deletes a Graph subscription, drops it from the store and prints the Graph result
func cmdDeleteSubscription(ctx context.Context, ns map[string]any) error {
	subscriptionID := textArg(ns, "subscription_id")
	store, err := openStore(ctx, ns)
	if err != nil {
		return err
	}
	client, err := cliGraphClient()
	if err != nil {
		return err
	}
	result, err := client.Delete(ctx, "/subscriptions/"+subscriptionID, nil)
	if err != nil {
		return err
	}
	if _, err := store.DeleteSubscription(subscriptionID); err != nil {
		return err
	}
	return printJSON(obj("subscription_id", subscriptionID, "result", result))
}

// Renews near expiry managed subscriptions and prints the maintenance report
func cmdMaintainSubscriptions(ctx context.Context, ns map[string]any) error {
	client, err := cliGraphClient()
	if err != nil {
		return err
	}
	store, err := openStore(ctx, ns)
	if err != nil {
		return err
	}
	renew, err := intArg(ns, "renew_within_hours", 24)
	if err != nil {
		return err
	}
	extend, err := intArg(ns, "extend_hours", 24)
	if err != nil {
		return err
	}
	var clientState *string
	if cs := textArg(ns, "client_state"); cs != "" {
		clientState = &cs
	}
	report, err := maintainGraphSubscriptions(ctx, client, store, renew, extend, pyrepr.Truthy(ns["dry_run"]), clientState)
	if err != nil {
		return err
	}
	return printJSON(report)
}

// Prints the Graph token cache health, refreshing the token first when asked
func cmdTokenHealth(ctx context.Context, ns map[string]any) error {
	provider, err := graphTokenProviderFromEnv()
	if err != nil {
		return err
	}
	payload := provider.InspectTokenHealth().Copy()
	if pyrepr.Truthy(ns["force_refresh"]) {
		token, err := provider.GetAccessToken(ctx, true)
		if err != nil {
			payload.Set("last_refresh_succeeded", false)
			payload.Set("refresh_error", err.Error())
		} else {
			payload.Set("last_refresh_succeeded", true)
			payload.Set("access_token_length", int64(pystr.Len(token)))
		}
	}
	return printJSON(payload)
}
