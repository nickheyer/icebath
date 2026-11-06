package cron

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// User facing path of a job's saved run output under the profile home
func cronOutputDirDisplay(h, jobID string) string {
	return home.Display(h) + "/cron/output/" + jobID + "/"
}

var (
	httpStatusInText = mustPyRe(`(?i)(?:\bHTTP\b|\bError code\b|\bstatus(?: code)?\b)\W{0,3}(\b[45]\d\d\b)`)
	leadingExcType   = mustPyRe(`^(?:[\w.]+\.)?([A-Z]\w*(?:Error|Timeout|Exception))\s*:`)
)

// Python re pattern compiled with CPython str semantics for its classes and boundaries
func mustPyRe(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// First group of the first match of a Python pattern, false when nothing matches
func pyReGroup(re *regexp2.Regexp, text string, group int) (string, bool) {
	m, err := re.FindStringMatch(text)
	if err != nil || m == nil {
		return "", false
	}
	return m.GroupByNumber(group).String(), true
}

var cronTransportErrorTypes = map[string]bool{
	"ReadTimeout": true, "ConnectTimeout": true, "PoolTimeout": true, "ConnectError": true, "RemoteProtocolError": true,
	"ConnectionError": true, "ConnectionResetError": true, "ConnectionAbortedError": true, "BrokenPipeError": true,
	"TimeoutError": true, "ReadError": true, "ServerDisconnectedError": true,
	"SSLError": true, "SSLZeroReturnError": true, "SSLWantReadError": true, "SSLWantWriteError": true, "SSLEOFError": true, "SSLSyscallError": true,
	"APIConnectionError": true, "APITimeoutError": true,
}

// Failure text rebuilt as an exception whose class name is the leading type prefix
type cronFailureText struct {
	name string
	text string
}

func (e *cronFailureText) Error() string { return e.text }

// Transport error rebuilt from text, classified by its class name like the Python transport type set
type cronTransportFailureText struct{ cronFailureText }

func (e *cronTransportFailureText) Timeout() bool   { return true }
func (e *cronTransportFailureText) Temporary() bool { return true }

// Failover reason value for a cron failure string, unknown when nothing classifies it
func classifyCronFailureReason(text string) string {
	in := provider.ClassifyInput{}
	name := "Exception"
	if m, ok := pyReGroup(leadingExcType, text, 1); ok {
		name = m
	}
	base := cronFailureText{name: name, text: text}
	if cronTransportErrorTypes[name] {
		in.Err = &cronTransportFailureText{base}
	} else {
		in.Err = &base
	}
	if m, ok := pyReGroup(httpStatusInText, text, 1); ok {
		in.Status, _ = strconv.Atoi(m)
	}
	return string(provider.ClassifyError(in).Reason)
}

// Plain language cause of a provider failure reason, false when the reason has no gloss
func providerFailureCause(reason string) (string, bool) {
	return tools.FailureCauseGloss(reason, "this job", "the job's")
}

var cronTransientReasons = map[string]bool{"timeout": true, "rate_limit": true, "upstream_rate_limit": true, "overloaded": true, "server_error": true}

var providerFailureAction = func() map[string]string {
	m := map[string]string{
		"billing": "Top up or wait for the limit to reset, or pin another provider with " +
			"`icebath cron edit {job_id} --provider <name>`.",
		"auth": "Sign in again with /login (or `{relogin}` in a terminal), or pin a " +
			"working provider with `icebath cron edit {job_id} --provider <name>`, then " +
			"`icebath cron run {job_id}` to retry.",
		"model_not_found": "Pick another model with `icebath cron edit {job_id} --model <name>`.",
		"upstream_blocked": "A firewall in front of the provider blocked the request (not your key): set a User-Agent " +
			"via `extra_headers` on the provider's custom_providers entry, or pin another provider with " +
			"`icebath cron edit {job_id} --provider <name>`.",
		"context_overflow": "Shorten the job's prompt with `icebath cron edit {job_id} --prompt <text>`.",
	}
	m["auth_permanent"] = m["auth"]
	m["billing_unverified"] = m["billing"]
	m["payload_too_large"] = m["context_overflow"]
	m["content_policy_blocked"] = "Reword the job's prompt with `icebath cron edit {job_id} --prompt <text>`, or pick another " +
		"model with `icebath cron edit {job_id} --model <name>`."
	m["provider_policy_blocked"] = "Retrying won't help: check the account's status and data/privacy settings with the provider, " +
		"or pin another model with `icebath cron edit {job_id} --model <name>`."
	return m
}()

const defaultFailureAction = "Run it again with `icebath cron run {job_id}`, or edit it with `icebath cron edit {job_id}`."

// One line provider failure notice with cause and fix, false when the reason has no gloss
func providerFailureNotice(h, jobName, jobID, reason, backupProviderPhrase string, prov any) (string, bool) {
	cause, ok := providerFailureCause(reason)
	if !ok {
		return "", false
	}
	var action string
	if cronTransientReasons[reason] {
		action = fmt.Sprintf("%s It will run again at its next scheduled time; `icebath cron run %s` tries now.", backupProviderPhrase, jobID)
	} else {
		tmpl, found := providerFailureAction[reason]
		if !found {
			tmpl = defaultFailureAction
		}
		provName := ""
		if p, isStr := prov.(string); isStr {
			provName = p
		}
		action = strings.NewReplacer("{job_id}", jobID, "{relogin}", provider.ReloginCommandHint(h, provName)).Replace(tmpl)
	}
	return fmt.Sprintf("⚠️ Cron '%s' failed: %s. %s Run log: `icebath cron runs %s`.", jobName, cause, action, jobID), true
}

// Unclassified failure notice with the cleaned error, where to look and what to do
func genericFailureNotice(h, jobName, jobID, cleanedError string) string {
	return fmt.Sprintf("⚠️ Cron '%s' failed: %s. ", jobName, cleanedError) +
		fmt.Sprintf("See the full run with `icebath cron runs %s` (output saved under ", jobID) +
		fmt.Sprintf("%s); run it again with `icebath cron run %s`, ", cronOutputDirDisplay(h, jobID), jobID) +
		fmt.Sprintf("edit it with `icebath cron edit %s`, or pause it with `icebath cron pause %s`.", jobID, jobID)
}

// Script timeout notice naming the output folder and the retry command
func scriptTimeoutNotice(h, jobName, jobID string) string {
	return fmt.Sprintf("⚠️ Cron '%s' failed: its script timed out. No model was invoked. ", jobName) +
		fmt.Sprintf("Check the script's output under %s or `icebath cron runs %s`, ", cronOutputDirDisplay(h, jobID), jobID) +
		fmt.Sprintf("then run it again with `icebath cron run %s`.", jobID)
}

// Inactivity cutoff notice naming the output folder and the retry command
func inactivityNotice(h, jobName, jobID string) string {
	return fmt.Sprintf("⚠️ Cron '%s' failed: the job stalled — it stopped doing anything for too long ", jobName) +
		"and was cut off. Check what it was doing in the saved output under " +
		fmt.Sprintf("%s (`icebath cron runs %s`), then run it again with ", cronOutputDirDisplay(h, jobID), jobID) +
		fmt.Sprintf("`icebath cron run %s`.", jobID)
}

// One time notice when the pre run configuration check refused to start the job
func blockedConfigNotice(jobName, reason string) string {
	reason = strings.TrimRight(reason, " \t\n\r\x0b\x0c")
	if reason != "" && !strings.ContainsAny(reason[len(reason)-1:], ".!?") {
		reason += "."
	}
	return fmt.Sprintf("⛔ Cron '%s' did not run: %s Nothing was charged. Icebath will try again at ", jobName, reason) +
		"the next scheduled time and will not repeat this alert; check with " +
		"`icebath cron doctor`."
}
