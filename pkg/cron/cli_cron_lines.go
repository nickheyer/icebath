package cron

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Catalog text under cli.commands in the active language, formatted with alternating keyword pairs
func cliT(ctx context.Context, key string, kv ...any) string {
	var kwargs map[string]any
	if len(kv) > 0 {
		kwargs = map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			kwargs[kv[i].(string)] = kv[i+1]
		}
	}
	return i18n.T(ctx, "cli.commands."+key, "", kwargs)
}

// Each line of a catalog value prefixed with pad, blank lines kept blank
func cliLines(text, pad string) []string {
	var out []string
	for _, line := range pystr.Splitlines(text, false) {
		if line == "" {
			out = append(out, "")
		} else {
			out = append(out, pad+line)
		}
	}
	return out
}

// Decoded reply of the cronjob tool called the way the CLI calls it
func (s *Store) cronAPI(ctx context.Context, a *cronjobArgs) *pyjson.Object {
	return cronAPIDoor(s, ctx, a)
}

// Cronjob tool call every CLI command makes, the name a case replaces the way pytest patches _cron_api
var cronAPIDoor = cronAPICall

// Runs the cronjob tool with the CLI defaults and decodes its JSON object reply
func cronAPICall(s *Store, ctx context.Context, a *cronjobArgs) *pyjson.Object {
	if a.Paused == nil {
		a.Paused = false
	}
	v, err := pyjson.Loads(s.cronjob(ctx, a))
	if err != nil {
		panic(err)
	}
	obj, _ := v.(*pyjson.Object)
	if obj == nil {
		panic(fmt.Sprintf("cronjob reply is not an object: %s", pyrepr.Repr(v)))
	}
	return obj
}

// Value of key in a decoded object, def when the key is absent
func field(o *pyjson.Object, key string, def any) any {
	if o != nil {
		if v, ok := o.Get(key); ok {
			return v
		}
	}
	return def
}

// Decoded job objects of a list reply, none unless the reply succeeded
func replyJobs(result *pyjson.Object) []*pyjson.Object {
	if !pyrepr.Truthy(field(result, "success", nil)) {
		return nil
	}
	items, _ := field(result, "jobs", nil).([]any)
	out := make([]*pyjson.Object, 0, len(items))
	for _, it := range items {
		if o, ok := it.(*pyjson.Object); ok {
			out = append(out, o)
		}
	}
	return out
}

// Skill names of a decoded job joined with a comma and a space
func joinedSkills(job *pyjson.Object) string {
	items, _ := field(job, "skills", nil).([]any)
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = pyrepr.Str(it)
	}
	return strings.Join(parts, ", ")
}

// Boxed overview with usage and the current jobs and their next runs
func (s *Store) cronOverviewLines(ctx context.Context) []string {
	rule := "+" + strings.Repeat("-", 68) + "+"
	out := []string{"", rule, "|" + pystr.Center(cliT(ctx, "cron.overview_title"), 68) + "|", rule, "", "  " + cliT(ctx, "cron.overview_commands")}
	out = append(out, cliLines(cliT(ctx, "cron.overview_usage"), "    ")...)
	out = append(out, "")
	jobs := replyJobs(s.cronAPI(ctx, &cronjobArgs{Action: "list"}))
	if len(jobs) == 0 {
		return append(out, "  "+cliT(ctx, "cron.no_jobs_hint"), "")
	}
	out = append(out, "  "+cliT(ctx, "cron.current_jobs"), "  "+strings.Repeat("-", 63))
	for _, job := range jobs {
		out = append(out, fmt.Sprintf("    %s | %s | %s", pystr.Ljust(pystr.Slice(pyrepr.Str(field(job, "job_id", nil)), 0, 12), 12),
			pystr.Ljust(pyrepr.Str(field(job, "schedule", nil)), 15), pystr.Ljust(pyrepr.Str(field(job, "repeat", "?")), 8)))
		if pyrepr.Truthy(field(job, "skills", nil)) {
			out = append(out, "      "+cliT(ctx, "cron.skills", "skills", joinedSkills(job)))
		}
		out = append(out, "      "+pyrepr.Str(field(job, "prompt_preview", "")))
		if pyrepr.Truthy(field(job, "next_run_at", nil)) {
			label, value := s.nextRunRow(job)
			if label == "Next run" {
				label = cliT(ctx, "cron.next_short")
			}
			out = append(out, fmt.Sprintf("      %s: %s", label, value))
		}
		out = append(out, "")
	}
	return append(out, "")
}

// Detailed job listing with state, schedule, next run, prompt and the last run verdict
func (s *Store) cronListLines(ctx context.Context, includeDisabled bool) []string {
	jobs := replyJobs(s.cronAPI(ctx, &cronjobArgs{Action: "list", IncludeDisabled: includeDisabled}))
	if len(jobs) == 0 {
		return []string{cliT(ctx, "cron.no_jobs")}
	}
	out := []string{"", cliT(ctx, "cron.list_header"), strings.Repeat("-", 80)}
	for _, job := range jobs {
		next := "  " + cliT(ctx, "cron.next_run_na")
		if pyrepr.Truthy(field(job, "next_run_at", nil)) {
			label, value := s.nextRunRow(job)
			next = fmt.Sprintf("  %s: %s", label, value)
		}
		out = append(out,
			"  "+cliT(ctx, "cron.id", "job_id", field(job, "job_id", nil)),
			"  "+cliT(ctx, "cron.name", "name", field(job, "name", nil)),
			"  "+cliT(ctx, "cron.state", "state", field(job, "state", "?")),
			"  "+cliT(ctx, "cron.schedule_repeat", "schedule", field(job, "schedule", nil), "repeat", field(job, "repeat", "?")),
			next)
		if pyrepr.Truthy(field(job, "skills", nil)) {
			out = append(out, "  "+cliT(ctx, "cron.skills", "skills", joinedSkills(job)))
		}
		out = append(out, "  "+cliT(ctx, "cron.prompt", "prompt", field(job, "prompt_preview", "")))
		if pyrepr.Truthy(field(job, "last_run_at", nil)) {
			status := field(job, "last_status", nil)
			if !pyrepr.Truthy(status) {
				status = "?"
			}
			if status == "delivery_failed" && pyrepr.Truthy(field(job, "last_delivery_error", nil)) {
				status = cliT(ctx, "cron.status_delivery_failed", "error", field(job, "last_delivery_error", nil))
			} else if status == "error" && pyrepr.Truthy(field(job, "last_error", nil)) {
				status = cliT(ctx, "cron.status_error", "error", field(job, "last_error", nil))
			}
			out = append(out, "  "+cliT(ctx, "cron.last_run", "when", field(job, "last_run_at", nil), "status", status))
		}
		out = append(out, "")
	}
	return out
}

var (
	cronBoolFlags  = map[string]string{"--clear-skills": "clear_skills", "--all": "all"}
	cronListFlags  = map[string]string{"--skill": "skills", "--add-skill": "add_skills", "--remove-skill": "remove_skills"}
	cronValueFlags = map[string]string{"--name": "name", "--deliver": "deliver", "--prompt": "prompt", "--schedule": "schedule"}
)

type cronFlags struct {
	values      map[string]*string
	repeat      any
	lists       map[string][]string
	clearSkills bool
	all         bool
	positionals []string
}

// Stripped non empty texts of values with duplicates dropped in first seen order
func cliNormalizeSkills(values []string) []string {
	var out []string
	for _, v := range values {
		text := pystr.Strip(v)
		if text != "" && !containsString(out, text) {
			out = append(out, text)
		}
	}
	return out
}

// Flags and positionals of a slash cron command, nil when repeat is not an integer
func parseCronFlags(tokens []string) *cronFlags {
	opts := &cronFlags{values: map[string]*string{}, lists: map[string][]string{}}
	for i := 0; i < len(tokens); {
		token := tokens[i]
		hasValue := i+1 < len(tokens)
		switch {
		case cronBoolFlags[token] == "clear_skills":
			opts.clearSkills = true
			i++
		case cronBoolFlags[token] == "all":
			opts.all = true
			i++
		case cronListFlags[token] != "" && hasValue:
			opts.lists[cronListFlags[token]] = append(opts.lists[cronListFlags[token]], tokens[i+1])
			i += 2
		case token == "--repeat" && hasValue:
			n, ok := pystr.IntText(tokens[i+1])
			if !ok {
				return nil
			}
			opts.repeat = n
			if n.IsInt64() {
				opts.repeat = int(n.Int64())
			}
			i += 2
		case cronValueFlags[token] != "" && hasValue:
			v := tokens[i+1]
			opts.values[cronValueFlags[token]] = &v
			i += 2
		default:
			opts.positionals = append(opts.positionals, token)
			i++
		}
	}
	return opts
}

// Optional flag value as the tool argument, None when absent
func flagArg(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Creates a job from the schedule and prompt positionals and reports its schedule and next run
func (s *Store) cronAddLines(ctx context.Context, _ string, opts *cronFlags) []string {
	if len(opts.positionals) == 0 {
		return []string{cliT(ctx, "cron.usage_add")}
	}
	schedule := opts.positionals[0]
	if v := opts.values["schedule"]; v != nil && *v != "" {
		schedule = *v
	}
	prompt := strings.Join(opts.positionals[1:], " ")
	if v := opts.values["prompt"]; v != nil && *v != "" {
		prompt = *v
	}
	skills := cliNormalizeSkills(opts.lists["skills"])
	if prompt == "" && len(skills) == 0 {
		return []string{cliT(ctx, "cron.need_prompt_or_skill")}
	}
	a := &cronjobArgs{Action: "create", Schedule: schedule, Name: flagArg(opts.values["name"]), Deliver: flagArg(opts.values["deliver"]), Repeat: opts.repeat}
	if prompt != "" {
		a.Prompt = prompt
	}
	if len(skills) > 0 {
		a.Skills = stringsToAny(skills)
	}
	result := s.cronAPI(ctx, a)
	if !pyrepr.Truthy(field(result, "success", nil)) {
		return []string{cliT(ctx, "cron.create_failed", "error", field(result, "error", nil))}
	}
	out := []string{cliT(ctx, "cron.created", "job_id", field(result, "job_id", nil)), "  " + cliT(ctx, "cron.schedule", "schedule", field(result, "schedule", nil))}
	if pyrepr.Truthy(field(result, "skills", nil)) {
		out = append(out, "  "+cliT(ctx, "cron.skills", "skills", joinedSkills(result)))
	}
	return append(out, "  "+cliT(ctx, "cron.next_run", "value", field(result, "next_run_at", nil)))
}

func stringsToAny(list []string) []any {
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}

// Updates a job with the given fields and the replaced, added or removed skills
func (s *Store) cronEditLines(ctx context.Context, _ string, opts *cronFlags) ([]string, error) {
	if len(opts.positionals) == 0 {
		return []string{cliT(ctx, "cron.usage_edit")}, nil
	}
	jobID := opts.positionals[0]
	existing, err := s.getJob(home.WithOverride(ctx, s.home), jobID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return []string{cliT(ctx, "cron.job_not_found", "job_id", jobID)}, nil
	}
	var finalSkills any
	replacement := cliNormalizeSkills(opts.lists["skills"])
	add := cliNormalizeSkills(opts.lists["add_skills"])
	remove := cliNormalizeSkills(opts.lists["remove_skills"])
	switch {
	case opts.clearSkills:
		finalSkills = []any{}
	case len(replacement) > 0:
		finalSkills = stringsToAny(replacement)
	case len(add) > 0 || len(remove) > 0:
		current := existing.Skills
		if len(current) == 0 {
			current = nil
			if existing.Skill != nil && *existing.Skill != "" {
				current = []string{*existing.Skill}
			}
		}
		kept := []string{}
		for _, sk := range current {
			if !containsString(remove, sk) {
				kept = append(kept, sk)
			}
		}
		for _, sk := range add {
			if !containsString(kept, sk) {
				kept = append(kept, sk)
			}
		}
		finalSkills = stringsToAny(kept)
	}
	result := s.cronAPI(ctx, &cronjobArgs{Action: "update", JobID: jobID, Schedule: flagArg(opts.values["schedule"]), Prompt: flagArg(opts.values["prompt"]),
		Name: flagArg(opts.values["name"]), Deliver: flagArg(opts.values["deliver"]), Repeat: opts.repeat, Skills: finalSkills})
	if !pyrepr.Truthy(field(result, "success", nil)) {
		return []string{cliT(ctx, "cron.update_failed", "error", field(result, "error", nil))}, nil
	}
	job, _ := field(result, "job", nil).(*pyjson.Object)
	skillsLine := "  " + cliT(ctx, "cron.skills_none")
	if pyrepr.Truthy(field(job, "skills", nil)) {
		skillsLine = "  " + cliT(ctx, "cron.skills", "skills", joinedSkills(job))
	}
	return []string{cliT(ctx, "cron.updated", "job_id", field(job, "job_id", nil)), "  " + cliT(ctx, "cron.schedule", "schedule", field(job, "schedule", nil)), skillsLine}, nil
}

// Pauses, resumes, runs or removes one job and reports the outcome
func (s *Store) cronJobActionLines(ctx context.Context, subcommand string, opts *cronFlags) []string {
	if len(opts.positionals) == 0 {
		return []string{cliT(ctx, "cron.usage_action", "subcommand", subcommand)}
	}
	jobID := opts.positionals[0]
	action := subcommand
	if subcommand == "remove" || subcommand == "rm" || subcommand == "delete" {
		action = "remove"
	}
	var reason any
	if action == "pause" {
		reason = "paused from /cron"
	}
	result := s.cronAPI(ctx, &cronjobArgs{Action: action, JobID: jobID, Reason: reason})
	if !pyrepr.Truthy(field(result, "success", nil)) {
		return []string{cliT(ctx, "cron."+action+"_failed", "error", field(result, "error", nil))}
	}
	if action == "remove" {
		removed, _ := field(result, "removed_job", nil).(*pyjson.Object)
		return []string{cliT(ctx, "cron.removed", "name", field(removed, "name", jobID), "job_id", jobID)}
	}
	job, _ := field(result, "job", nil).(*pyjson.Object)
	if action == "run" && pyrepr.Truthy(field(job, "execution_skipped", nil)) {
		return []string{cliT(ctx, "cron.did_not_run", "name", field(job, "name", nil), "job_id", jobID, "reason", field(job, "execution_skipped", nil))}
	}
	key := map[string]string{"pause": "cron.paused", "resume": "cron.resumed", "run": "cron.triggered"}[action]
	out := []string{cliT(ctx, key, "name", field(job, "name", nil), "job_id", jobID)}
	switch action {
	case "resume":
		out = append(out, "  "+cliT(ctx, "cron.next_run", "value", field(job, "next_run_at", nil)))
	case "run":
		out = append(out, "  "+runOutcome(job))
	}
	return out
}

// Output lines of the slash cron command, the overview when no subcommand is given
func (s *Store) cronCommandLines(ctx context.Context, arg string) ([]string, error) {
	tokens, err := pyshlex.SplitPOSIX(arg, false)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return s.cronOverviewLines(ctx), nil
	}
	subcommand := strings.ToLower(tokens[0])
	opts := parseCronFlags(tokens[1:])
	if opts == nil {
		return []string{cliT(ctx, "cron.repeat_must_be_integer")}, nil
	}
	switch subcommand {
	case "list":
		return s.cronListLines(ctx, opts.all || containsString(opts.positionals, "-a")), nil
	case "add", "create":
		return s.cronAddLines(ctx, subcommand, opts), nil
	case "edit":
		return s.cronEditLines(ctx, subcommand, opts)
	case "pause", "resume", "run", "remove", "rm", "delete":
		return s.cronJobActionLines(ctx, subcommand, opts), nil
	}
	return []string{cliT(ctx, "cron.unknown_command", "subcommand", subcommand), "  " + cliT(ctx, "cron.available")}, nil
}

// Output lines of the slash cron command in the cron store of home h
func CronCommandLines(ctx context.Context, h, arg string) ([]string, error) {
	return openCronStore(h, nil).cronCommandLines(ctx, arg)
}
