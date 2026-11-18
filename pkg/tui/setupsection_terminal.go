package tui

import (
	"os"
	"slices"
	"strings"
)

// Built in terminal backends in menu order
var terminalBuiltins = []string{"local", "docker", "modal", "ssh", "daytona", "vercel_sandbox"}

// Registered backends the gateway listed beyond the built in ones, name, display name and description
func terminalPluginRows(run *sectionRun) [][3]string {
	var rows [][3]string
	list, _ := run.values["terminal.plugin_backends"].([]any)
	for _, raw := range list {
		row, _ := raw.(map[string]any)
		name, _ := row["name"].(string)
		display, _ := row["display_name"].(string)
		description, _ := row["description"].(string)
		rows = append(rows, [3]string{name, display, description})
	}
	return rows
}

// Backend picked in this walk, empty when the current one is kept
func terminalPicked(run *sectionRun) string {
	picked, _ := run.answers["backend"].(string)
	return picked
}

// Whether a registered backend outside the built in ones holds the name
func terminalPluginNamed(run *sectionRun, name string) bool {
	for _, row := range terminalPluginRows(run) {
		if row[0] == name {
			return true
		}
	}
	return false
}

// Condition true when the walk picked the named built in backend and no registered backend shadows its ssh name
func terminalRuns(name string) sectionCond {
	return func(run *sectionRun) bool {
		return terminalPicked(run) == name && (name != "ssh" || !terminalPluginNamed(run, name))
	}
}

// Condition true when the picked backend is a registered one the built in steps do not handle
func terminalPluginRuns(run *sectionRun) bool {
	picked := terminalPicked(run)
	if picked == "" || !terminalPluginNamed(run, picked) {
		return false
	}
	return !slices.Contains(terminalBuiltins, picked) || picked == "ssh"
}

// Backend config.yaml holds, local when it names none
func terminalCurrent(run *sectionRun) string {
	if current := sectionText(run.held["terminal.backend"]); run.held["terminal.backend"] != nil && current != "" {
		return current
	}
	return "local"
}

// Backend menu, the built in backends, the registered ones and a row that keeps the current backend
func terminalBackendChoices(run *sectionRun) []sectionChoice {
	choices := []sectionChoice{}
	for _, name := range terminalBuiltins {
		choices = append(choices, sectionChoice{name, tr("setupFlow.terminal.backend." + name), tr("setupFlow.terminal.backend." + name + "Detail")})
	}
	for _, row := range terminalPluginRows(run) {
		choices = append(choices, sectionChoice{row[0], row[1], row[2]})
	}
	return append(choices, sectionChoice{nil, tr("setupFlow.terminal.keep", terminalCurrent(run)), tr("setupFlow.terminal.keepDetail")})
}

// Text value of a gateway value map entry, empty when absent
func sectionValueText(run *sectionRun, name, key string) string {
	entry, _ := run.values[name].(map[string]any)
	text, _ := entry[key].(string)
	return text
}

// Starting answer from the .env value of a key, else the fallback
func envOr(key string, fallback func(run *sectionRun) string) func(run *sectionRun) any {
	return func(run *sectionRun) any {
		if held := sectionText(run.env[key]); run.env[key] != nil && held != "" {
			return held
		}
		return fallback(run)
	}
}

// Fallback of a fixed text
func fixedText(text string) func(*sectionRun) string { return func(*sectionRun) string { return text } }

// Settled answer that drops an empty text and every listed text
func dropTexts(dropped ...string) func(*sectionRun, any) any {
	return func(_ *sectionRun, value any) any {
		if text := sectionText(value); text == "" || slices.Contains(dropped, text) {
			return nil
		}
		return value
	}
}

// Condition true when the .env key a secret field targets holds a value
func secretHeld(key string) sectionCond {
	return func(run *sectionRun) bool { return run.secrets[key] }
}

// Condition true when the field has an answer
func answered(id string) sectionCond {
	return func(run *sectionRun) bool { return run.answers[id] != nil }
}

// Condition true when any listed condition holds
func anyOf(conds ...sectionCond) sectionCond {
	return func(run *sectionRun) bool {
		for _, cond := range conds {
			if cond(run) {
				return true
			}
		}
		return false
	}
}

// Yes and no options of a question whose answer only steers the walk
func yesNo(prefix string) []sectionChoice {
	return []sectionChoice{
		{true, tr(prefix + ".yes"), tr(prefix + ".yesDetail")},
		{false, tr(prefix + ".no"), tr(prefix + ".noDetail")},
	}
}

// Python truth of a loaded config value, absent and null reading false
func sectionTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	n, isNumber := sectionFloat(value)
	return !isNumber || n != 0
}

// Fixed starting answer of a field
func seedOf(value any) func(*sectionRun) any { return func(*sectionRun) any { return value } }

// Billing the Modal menu starts on, the saved mode, else direct when a Modal token is saved
func modalBillingSeed(run *sectionRun) any {
	mode := strings.ToLower(strings.TrimSpace(sectionText(run.held["terminal.modal_mode"])))
	if run.held["terminal.modal_mode"] == nil || mode != "direct" && mode != "managed" {
		mode = ""
	}
	if mode == "direct" || mode != "managed" && run.secrets["MODAL_TOKEN_ID"] {
		return "direct"
	}
	return "managed"
}

// Whether the gateway found the managed Nous route to Modal
func modalManaged(run *sectionRun) bool { return run.values["terminal.modal_managed"] == true }

// Questions of the Docker backend, the runtime found and the egress firewall offer
func terminalDockerFields() []sectionField {
	docker := terminalRuns("docker")
	return []sectionField{
		{id: "docker", kind: fieldNotice, when: docker, labelFrom: func(run *sectionRun) string {
			if path := sectionValueText(run, "terminal.docker", "path"); path != "" {
				return tr("setupFlow.terminal.dockerFound", sectionValueText(run, "terminal.docker", "runtime"), path)
			}
			return tr("setupFlow.terminal.dockerMissing")
		}},
		{id: "egress_intro", kind: fieldNotice, label: tr("setupFlow.terminal.egressIntro"), when: docker},
		{id: "egress", kind: fieldChoice, label: tr("setupFlow.terminal.egressLabel"), when: docker, seed: seedOf(false), choices: yesNo("setupFlow.terminal.egress")},
		{id: "egress_on", kind: fieldNotice, label: tr("setupFlow.terminal.egressOn"), when: answerIs("egress", true)},
		{id: "egress_off", kind: fieldNotice, label: tr("setupFlow.terminal.egressOff"), when: answerIs("egress", false)},
	}
}

// Questions of the Modal backend, managed or direct billing and the direct token pair
func terminalModalFields() []sectionField {
	modal := terminalRuns("modal")
	direct := allOf(modal, notCond(answerIs("modal_billing", "managed")))
	held := secretHeld("MODAL_TOKEN_ID")
	asks := allOf(direct, anyOf(notCond(held), answerIs("modal_update", true)))
	return []sectionField{
		{id: "modal", kind: fieldNotice, label: tr("setupFlow.terminal.modal"), when: modal},
		{id: "modal_billing", kind: fieldChoice, label: tr("setupFlow.terminal.modalBillingLabel"), targets: []sectionTarget{{key: "terminal.modal_mode"}},
			when: allOf(modal, modalManaged),
			seed: modalBillingSeed, choices: []sectionChoice{
				{"managed", tr("setupFlow.terminal.modalBilling.managed"), tr("setupFlow.terminal.modalBilling.managedDetail")},
				{"direct", tr("setupFlow.terminal.modalBilling.direct"), tr("setupFlow.terminal.modalBilling.directDetail")}}},
		{id: "modal_managed", kind: fieldNotice, label: tr("setupFlow.terminal.modalManaged"), when: answerIs("modal_billing", "managed")},
		{id: "modal_direct_creds", kind: fieldNotice, label: tr("setupFlow.terminal.modalDirectCreds"),
			when: allOf(answerIs("modal_billing", "managed"), anyOf(held, secretHeld("MODAL_TOKEN_SECRET")))},
		{id: "modal_direct", kind: fieldNotice, label: tr("setupFlow.terminal.modalDirect"), when: direct},
		{id: "modal_held", kind: fieldNotice, label: tr("setupFlow.terminal.modalHeld"), when: allOf(direct, held)},
		{id: "modal_update", kind: fieldChoice, label: tr("setupFlow.terminal.modalUpdateLabel"), when: allOf(direct, held), seed: seedOf(false),
			choices: yesNo("setupFlow.terminal.update")},
		{id: "modal_token_id", kind: fieldSecret, label: tr("setupFlow.terminal.modalTokenID"), targets: []sectionTarget{{env: true, key: "MODAL_TOKEN_ID"}}, when: asks},
		{id: "modal_token_secret", kind: fieldSecret, label: tr("setupFlow.terminal.modalTokenSecret"), targets: []sectionTarget{{env: true, key: "MODAL_TOKEN_SECRET"}}, when: asks},
	}
}

// Questions of the SSH backend, host, user, port and key with the connection test
func terminalSSHFields() []sectionField {
	ssh := terminalRuns("ssh")
	env := func(key string) []sectionTarget { return []sectionTarget{{env: true, key: key}} }
	defaults := func(key string) func(*sectionRun) string {
		return func(run *sectionRun) string { return sectionValueText(run, "terminal.ssh_defaults", key) }
	}
	return []sectionField{
		{id: "ssh", kind: fieldNotice, label: tr("setupFlow.terminal.ssh"), when: ssh},
		{id: "ssh_host", kind: fieldText, label: tr("setupFlow.terminal.sshHost"), targets: env("TERMINAL_SSH_HOST"), when: ssh,
			seed: envOr("TERMINAL_SSH_HOST", fixedText("")), settle: dropTexts()},
		{id: "ssh_user", kind: fieldText, label: tr("setupFlow.terminal.sshUser"), targets: env("TERMINAL_SSH_USER"), when: ssh,
			seed: envOr("TERMINAL_SSH_USER", defaults("user")), settle: dropTexts()},
		{id: "ssh_port", kind: fieldText, label: tr("setupFlow.terminal.sshPort"), targets: env("TERMINAL_SSH_PORT"), when: ssh,
			seed: envOr("TERMINAL_SSH_PORT", fixedText("22")), settle: dropTexts("22")},
		{id: "ssh_key", kind: fieldText, label: tr("setupFlow.terminal.sshKey"), targets: env("TERMINAL_SSH_KEY"), when: ssh,
			seed: envOr("TERMINAL_SSH_KEY", defaults("key")), settle: dropTexts()},
		{id: "ssh_test", kind: fieldChoice, label: tr("setupFlow.terminal.sshTestLabel"), seed: seedOf(true), choices: yesNo("setupFlow.terminal.sshTest"),
			when: allOf(ssh, func(run *sectionRun) bool { return sectionText(run.answers["ssh_host"]) != "" })},
		{id: "ssh_probe", kind: fieldAction, action: "terminal.ssh.test", label: tr("setupFlow.terminal.sshTesting"), when: answerIs("ssh_test", true),
			params: func(run *sectionRun) map[string]any {
				return map[string]any{"host": sectionText(run.answers["ssh_host"]), "user": sectionText(run.answers["ssh_user"]),
					"port": sectionText(run.answers["ssh_port"]), "key": sectionText(run.answers["ssh_key"])}
			}},
	}
}

// Questions of the Daytona backend, the API key kept or replaced
func terminalDaytonaFields() []sectionField {
	daytona := terminalRuns("daytona")
	held := secretHeld("DAYTONA_API_KEY")
	return []sectionField{
		{id: "daytona", kind: fieldNotice, label: tr("setupFlow.terminal.daytona"), when: daytona},
		{id: "daytona_held", kind: fieldNotice, label: tr("setupFlow.terminal.daytonaHeld"), when: allOf(daytona, held)},
		{id: "daytona_update", kind: fieldChoice, label: tr("setupFlow.terminal.daytonaUpdateLabel"), when: allOf(daytona, held), seed: seedOf(false),
			choices: yesNo("setupFlow.terminal.update")},
		{id: "daytona_key", kind: fieldSecret, label: tr("setupFlow.terminal.daytonaKey"), targets: []sectionTarget{{env: true, key: "DAYTONA_API_KEY"}},
			when: allOf(daytona, anyOf(notCond(held), answerIs("daytona_update", true)))},
		{id: "daytona_updated", kind: fieldNotice, label: tr("setupFlow.terminal.daytonaUpdated"), when: allOf(answered("daytona_key"), held)},
		{id: "daytona_configured", kind: fieldNotice, label: tr("setupFlow.terminal.daytonaConfigured"), when: allOf(answered("daytona_key"), notCond(held))},
	}
}

// Questions of the Vercel Sandbox backend, image, persistence, resources and access token auth
func terminalVercelFields(linkedValue string) []sectionField {
	vercel := terminalRuns("vercel_sandbox")
	env := func(key string) []sectionTarget { return []sectionTarget{{env: true, key: key}} }
	linked := func(key string) func(*sectionRun) string {
		return func(run *sectionRun) string { return sectionValueText(run, linkedValue, key) }
	}
	return []sectionField{
		{id: "vercel", kind: fieldNotice, label: tr("setupFlow.terminal.vercel"), when: vercel},
		{id: "vercel_image", kind: fieldText, label: tr("setupFlow.terminal.vercelImage"), when: vercel,
			targets: []sectionTarget{{key: "terminal.vercel_image"}, {env: true, key: "TERMINAL_VERCEL_IMAGE"}},
			seed: func(run *sectionRun) any {
				if image := sectionText(run.held["terminal.vercel_image"]); run.held["terminal.vercel_image"] != nil && image != "" {
					return image
				}
				return run.values["terminal.vercel_default_image"]
			}},
		{id: "vercel_runtime", kind: fieldNotice, when: allOf(vercel, func(run *sectionRun) bool { return sectionTruthy(run.held["terminal.vercel_runtime"]) }),
			labelFrom: func(run *sectionRun) string {
				return tr("setupFlow.terminal.vercelRuntime", sectionText(run.held["terminal.vercel_runtime"]))
			}},
		{id: "vercel_persist", kind: fieldChoice, label: tr("setupFlow.terminal.vercelPersistLabel"), targets: []sectionTarget{{key: "terminal.container_persistent"}},
			when: vercel, choices: yesNo("setupFlow.terminal.persist"), seed: func(run *sectionRun) any {
				return sectionTruthy(run.held["terminal.container_persistent"])
			}},
		{id: "vercel_cpu", kind: fieldNumber, label: tr("setupFlow.terminal.vercelCPU"), targets: []sectionTarget{{key: "terminal.container_cpu"}}, fallback: 1, when: vercel},
		{id: "vercel_memory", kind: fieldNumber, whole: true, label: tr("setupFlow.terminal.vercelMemory"), targets: []sectionTarget{{key: "terminal.container_memory"}},
			fallback: 5120, when: vercel},
		{id: "vercel_disk", kind: fieldNotice, label: tr("setupFlow.terminal.vercelDisk"), when: allOf(vercel, func(run *sectionRun) bool {
			disk, found := sectionFloat(run.held["terminal.container_disk"])
			if flag, isBool := run.held["terminal.container_disk"].(bool); isBool {
				disk, found = map[bool]float64{false: 0, true: 1}[flag], true
			}
			return !found || disk != 0 && disk != 51200
		})},
		{id: "vercel_auth", kind: fieldNotice, label: tr("setupFlow.terminal.vercelAuth"), when: vercel},
		{id: "vercel_linked", kind: fieldNotice, label: tr("setupFlow.terminal.vercelLinked"), when: allOf(vercel, func(run *sectionRun) bool {
			entry, _ := run.values[linkedValue].(map[string]any)
			return len(entry) > 0
		})},
		{id: "vercel_token", kind: fieldSecret, label: tr("setupFlow.terminal.vercelToken"), targets: env("VERCEL_TOKEN"), when: vercel,
			seed: func(run *sectionRun) any {
				if run.secrets["VERCEL_TOKEN"] {
					return sectionHeld{}
				}
				return nil
			}},
		{id: "vercel_project", kind: fieldText, label: tr("setupFlow.terminal.vercelProject"), targets: env("VERCEL_PROJECT_ID"), when: vercel,
			seed: envOr("VERCEL_PROJECT_ID", linked("projectId")), settle: dropTexts()},
		{id: "vercel_team", kind: fieldText, label: tr("setupFlow.terminal.vercelTeam"), targets: env("VERCEL_TEAM_ID"), when: vercel,
			seed: envOr("VERCEL_TEAM_ID", linked("orgId")), settle: dropTexts()},
	}
}

// Where shell commands run, the backend menu then the questions of the picked backend, saved as the wizard saves
func terminalSection() sectionSpec {
	cwd, _ := os.Getwd()
	linkedValue := "terminal.vercel_linked:" + cwd
	fields := []sectionField{
		{id: "backend", kind: fieldChoice, label: tr("setupFlow.terminal.backendLabel"), seed: seedOf(nil), choicesFrom: terminalBackendChoices,
			targets: []sectionTarget{{key: "terminal.backend"}}},
		{id: "kept", kind: fieldNotice, when: answerIs("backend", nil),
			labelFrom: func(run *sectionRun) string { return tr("setupFlow.terminal.kept", terminalCurrent(run)) }},
		{id: "local", kind: fieldNotice, label: tr("setupFlow.terminal.local"), when: terminalRuns("local")},
	}
	fields = append(fields, terminalDockerFields()...)
	fields = append(fields, terminalModalFields()...)
	fields = append(fields, terminalSSHFields()...)
	fields = append(fields, terminalDaytonaFields()...)
	fields = append(fields, terminalVercelFields(linkedValue)...)
	modalDirectOnly := allOf(terminalRuns("modal"), notCond(modalManaged))
	fields = append(fields,
		sectionField{id: "plugin", kind: fieldAction, action: "terminal.backend.plugin", label: tr("setupFlow.terminal.pluginSetup"),
			when: terminalPluginRuns, params: func(run *sectionRun) map[string]any {
				return map[string]any{"backend": terminalPicked(run), "cwd": cwd}
			}},
		sectionField{id: "backend_env", kind: fieldDerived, targets: []sectionTarget{{env: true, key: "TERMINAL_ENV"}}, when: answered("backend"),
			seed: func(run *sectionRun) any { return run.answers["backend"] }},
		sectionField{id: "modal_mode_env", kind: fieldDerived, targets: []sectionTarget{{env: true, key: "TERMINAL_MODAL_MODE"}}, when: terminalRuns("modal"),
			seed: func(run *sectionRun) any {
				if mode := run.answers["modal_billing"]; mode != nil {
					return mode
				}
				return "direct"
			}})
	vercel := terminalRuns("vercel_sandbox")
	return sectionSpec{key: "terminal", title: tr("setupFlow.terminal.title"), detail: tr("setupFlow.terminal.detail"), intro: tr("setupFlow.terminal.intro"),
		fields: fields, fullSave: true,
		values: []string{"terminal.plugin_backends", "terminal.docker", "terminal.modal_managed", "terminal.ssh_defaults", linkedValue,
			"terminal.vercel_default_image"},
		reads: []sectionTarget{{key: "terminal.vercel_runtime"}, {key: "terminal.container_disk"}},
		constants: []sectionConstant{
			{target: sectionTarget{key: "proxy.enabled"}, value: true, when: answerIs("egress", true)},
			{target: sectionTarget{key: "terminal.modal_mode"}, value: "direct", when: modalDirectOnly},
			{target: sectionTarget{env: true, key: "TERMINAL_SSH_PORT"}, when: allOf(terminalRuns("ssh"), answerIs("ssh_port", "22"))},
			{target: sectionTarget{key: "terminal.container_disk"}, value: 51200, when: vercel},
			{target: sectionTarget{env: true, key: "VERCEL_OIDC_TOKEN"}, when: vercel},
		}}
}
