package tui

import (
	"runtime"
	"strings"
)

// Keyed speech providers in menu order
var ttsKeyed = []string{"elevenlabs", "openai", "minimax", "mistral", "gemini"}

// Env keys that satisfy each keyed speech provider, a pasted key going to the first
var ttsKeyEnvs = map[string][]string{
	"elevenlabs": {"ELEVENLABS_API_KEY"},
	"openai":     {"VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY"},
	"minimax":    {"MINIMAX_API_KEY"},
	"mistral":    {"MISTRAL_API_KEY"},
	"gemini":     {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
}

// Speech provider picked in this walk, empty when the current one is kept
func ttsPicked(run *sectionRun) string {
	if picked, answered := run.answers["provider_nous"].(string); answered {
		return picked
	}
	picked, _ := run.answers["provider"].(string)
	return picked
}

// Condition true when the walk picked the named speech provider
func ttsPickedIs(name string) sectionCond {
	return func(run *sectionRun) bool { return ttsPicked(run) == name }
}

// Condition true when an env key that satisfies the keyed speech provider holds a value
func ttsKeyHeld(name string) sectionCond {
	return func(run *sectionRun) bool {
		for _, key := range ttsKeyEnvs[name] {
			if run.facts["env:"+key] {
				return true
			}
		}
		return false
	}
}

// Speech provider the walk ends on, edge when the picked one lacks its key, login or engine
func ttsOutcome(run *sectionRun) string {
	picked := ttsPicked(run)
	fell := false
	switch picked {
	case "nous-openai":
		return "openai"
	case "xai":
		fell = !run.facts["env:XAI_API_KEY"] && !run.facts["xai.oauth"] && run.answers["xai_login"] != true && run.answers["xai_key"] == nil
	case "neutts", "kittentts":
		fell = run.facts["extra:"+picked] && !run.facts["module:"+picked] && run.answers["sync_"+picked] != true
	default:
		if _, keyed := ttsKeyEnvs[picked]; keyed {
			fell = !ttsKeyHeld(picked)(run) && run.answers["key_"+picked] == nil
		}
	}
	if fell {
		return "edge"
	}
	return picked
}

// Condition true when the walk ends on the named speech provider
func ttsOutcomeIs(name string) sectionCond {
	return func(run *sectionRun) bool { return ttsOutcome(run) == name }
}

// Command that installs espeak-ng with the package manager of this platform
func espeakInstall() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"brew", "install", "espeak-ng"}
	case "windows":
		return []string{"choco", "install", "espeak-ng", "-y"}
	}
	return []string{"sudo", "apt", "install", "-y", "espeak-ng"}
}

// Speech provider menu, the managed Nous route first when the subscription offers it
func ttsProviders(nous bool) []sectionChoice {
	choices := []sectionChoice{{nil, tr("setupFlow.tts.keep"), tr("setupFlow.tts.keepDetail")}}
	if nous {
		choices = append(choices, sectionChoice{"nous-openai", "Nous Subscription", tr("setupFlow.tts.provider.nous")})
	}
	return append(choices,
		sectionChoice{"edge", "Edge TTS", tr("setupFlow.tts.provider.edge")},
		sectionChoice{"elevenlabs", "ElevenLabs", tr("setupFlow.tts.provider.elevenlabs")},
		sectionChoice{"openai", "OpenAI TTS", tr("setupFlow.tts.provider.openai")},
		sectionChoice{"xai", "xAI TTS", tr("setupFlow.tts.provider.xai")},
		sectionChoice{"minimax", "MiniMax TTS", tr("setupFlow.tts.provider.minimax")},
		sectionChoice{"mistral", "Mistral Voxtral TTS", tr("setupFlow.tts.provider.mistral")},
		sectionChoice{"gemini", "Google Gemini TTS", tr("setupFlow.tts.provider.gemini")},
		sectionChoice{"neutts", "NeuTTS", tr("setupFlow.tts.provider.neutts")},
		sectionChoice{"kittentts", "KittenTTS", tr("setupFlow.tts.provider.kittentts")})
}

// Questions of one local speech engine, its platform support, install offer, system package and install run
func ttsEngineFields(engine, name string) []sectionField {
	picked, supported, present := ttsPickedIs(engine), factHolds("extra:"+engine), factHolds("module:"+engine)
	offered := allOf(picked, supported, notCond(present))
	install, sync := "install_"+engine, "sync_"+engine
	fields := []sectionField{
		{id: engine + "_unsupported", kind: fieldNotice, label: tr("setupFlow.tts.unsupported", name), when: allOf(picked, notCond(supported))},
		{id: engine + "_present", kind: fieldNotice, label: tr("setupFlow.tts.installed", name), when: allOf(picked, supported, present)},
		{id: engine + "_needs", kind: fieldNotice, label: tr("setupFlow.tts." + engine + "Needs"), when: offered},
		{id: install, kind: fieldChoice, label: tr("setupFlow.tts.installLabel"), when: offered, choices: []sectionChoice{
			{true, tr("setupFlow.tts.install.yes"), tr("setupFlow.tts.install.yesDetail")},
			{false, tr("setupFlow.tts.install.no"), tr("setupFlow.tts.install.noDetail")}}},
		{id: engine + "_skipped", kind: fieldNotice, label: tr("setupFlow.tts.installSkipped", engine), when: answerIs(install, false)},
	}
	ready := answerIs(install, true)
	if engine == "neutts" {
		argv := espeakInstall()
		missing := allOf(ready, notCond(factHolds("binary:espeak-ng")), notCond(factHolds("binary:espeak")))
		fields = append(fields,
			sectionField{id: "espeak_needed", kind: fieldNotice, label: tr("setupFlow.tts.espeakNeeded", strings.Join(argv, " ")), when: missing},
			sectionField{id: "espeak", kind: fieldChoice, label: tr("setupFlow.tts.espeakLabel"), when: missing, choices: []sectionChoice{
				{true, tr("setupFlow.tts.espeak.yes"), tr("setupFlow.tts.espeak.yesDetail")},
				{false, tr("setupFlow.tts.espeak.no"), tr("setupFlow.tts.espeak.noDetail")}}},
			sectionField{id: "espeak_run", kind: fieldCommand, argv: argv, when: answerIs("espeak", true)},
			sectionField{id: "espeak_done", kind: fieldNotice, label: tr("setupFlow.tts.espeakDone"), when: answerIs("espeak_run", true)},
			sectionField{id: "espeak_failed", kind: fieldNotice, label: tr("setupFlow.tts.espeakFailed"), when: answerIs("espeak_run", false)},
			sectionField{id: "espeak_skipped", kind: fieldNotice, label: tr("setupFlow.tts.espeakSkipped"), when: answerIs("espeak", false)})
		ready = allOf(ready, notCond(answerIs("espeak_run", false)))
	}
	return append(fields,
		sectionField{id: sync, kind: fieldAction, action: "tts.install." + engine, label: tr("setupFlow.tts.installing"), when: ready},
		sectionField{id: engine + "_failed", kind: fieldNotice, label: tr("setupFlow.tts.installFailed", name),
			when: allOf(answerIs(install, true), notCond(answerIs(sync, true)))})
}

// Speech provider with its key, login or local engine, then the voice of the provider the walk ends on
func ttsSection() sectionSpec {
	provider := []sectionTarget{{key: "tts.provider"}}
	settled := func(run *sectionRun, _ any) any { return ttsOutcome(run) }
	nous := factHolds("nous.managed_tools")
	facts := []string{"nous.managed_tools", "xai.oauth", "env:XAI_API_KEY", "extra:neutts", "extra:kittentts", "module:neutts", "module:kittentts",
		"binary:espeak-ng", "binary:espeak"}
	fields := []sectionField{
		{id: "provider", kind: fieldChoice, label: tr("setupFlow.tts.providerLabel"), targets: provider, fallback: "edge",
			choices: ttsProviders(false), when: notCond(nous), settle: settled},
		{id: "provider_nous", kind: fieldChoice, label: tr("setupFlow.tts.providerLabel"), targets: provider, fallback: "edge",
			choices: ttsProviders(true), when: nous, settle: settled},
		{id: "nous_managed", kind: fieldNotice, label: tr("setupFlow.tts.nousManaged"), when: ttsPickedIs("nous-openai")},
		{id: "nous_direct", kind: fieldNotice, label: tr("setupFlow.tts.nousDirect"), when: allOf(ttsPickedIs("nous-openai"), ttsKeyHeld("openai"))},
	}
	for _, name := range ttsKeyed {
		for _, key := range ttsKeyEnvs[name] {
			facts = append(facts, "env:"+key)
		}
		asked := allOf(ttsPickedIs(name), notCond(ttsKeyHeld(name)))
		if name == "gemini" {
			fields = append(fields, sectionField{id: "gemini_hint", kind: fieldNotice, label: tr("setupFlow.tts.geminiHint"), when: asked})
		}
		fields = append(fields,
			sectionField{id: "key_" + name, kind: fieldSecret, label: tr("setupFlow.tts.keyLabel." + name),
				targets: []sectionTarget{{env: true, key: ttsKeyEnvs[name][0]}}, when: asked},
			sectionField{id: "no_key_" + name, kind: fieldNotice, label: tr("setupFlow.tts.noKey"), when: allOf(asked, answerIs("key_"+name, nil))})
	}
	xai := ttsPickedIs("xai")
	xaiKey, xaiOAuth := factHolds("env:XAI_API_KEY"), factHolds("xai.oauth")
	fields = append(fields,
		sectionField{id: "xai_key_held", kind: fieldNotice, label: tr("setupFlow.tts.xaiKeyHeld"), when: allOf(xai, xaiKey)},
		sectionField{id: "xai_oauth_held", kind: fieldNotice, label: tr("setupFlow.tts.xaiOAuthHeld"), when: allOf(xai, notCond(xaiKey), xaiOAuth)},
		sectionField{id: "xai_auth", kind: fieldChoice, label: tr("setupFlow.tts.xaiAuthLabel"), when: allOf(xai, notCond(xaiKey), notCond(xaiOAuth)),
			choices: []sectionChoice{
				{"oauth", tr("setupFlow.tts.xaiAuth.oauth"), tr("setupFlow.tts.xaiAuth.oauthDetail")},
				{"key", tr("setupFlow.tts.xaiAuth.key"), tr("setupFlow.tts.xaiAuth.keyDetail")},
				{"skip", tr("setupFlow.tts.xaiAuth.skip"), tr("setupFlow.tts.xaiAuth.skipDetail")}}},
		sectionField{id: "xai_login", kind: fieldAction, action: "xai.oauth.login", label: tr("setupFlow.tts.xaiLogin"), when: answerIs("xai_auth", "oauth")},
		sectionField{id: "xai_login_done", kind: fieldNotice, label: tr("setupFlow.tts.xaiLoginDone"), when: answerIs("xai_login", true)},
		sectionField{id: "xai_login_failed", kind: fieldNotice, label: tr("setupFlow.tts.xaiLoginFailed"), when: answerIs("xai_login", false)},
		sectionField{id: "xai_key", kind: fieldSecret, label: tr("setupFlow.tts.xaiKeyLabel"),
			targets: []sectionTarget{{env: true, key: "XAI_API_KEY"}}, when: answerIs("xai_auth", "key")},
		sectionField{id: "xai_no_key", kind: fieldNotice, label: tr("setupFlow.tts.xaiNoKey"), when: allOf(answerIs("xai_auth", "key"), answerIs("xai_key", nil))},
		sectionField{id: "xai_skipped", kind: fieldNotice, label: tr("setupFlow.tts.xaiSkipped"), when: answerIs("xai_auth", "skip")},
		sectionField{id: "xai_voice", kind: fieldText, label: tr("setupFlow.tts.xaiVoice"),
			targets: []sectionTarget{{key: "tts.xai.voice_id"}}, when: ttsOutcomeIs("xai"), seed: seedOf(nil)})
	fields = append(fields, ttsEngineFields("neutts", "NeuTTS")...)
	fields = append(fields, ttsEngineFields("kittentts", "KittenTTS")...)
	voice := []sectionTarget{{key: "tts.edge.voice"}}
	fields = append(fields,
		sectionField{id: "edge_voice", kind: fieldChoice, label: tr("setupFlow.tts.voiceLabel"), targets: voice, fallback: "en-US-AriaNeural",
			when: ttsOutcomeIs("edge"), choices: []sectionChoice{
				{nil, tr("setupFlow.tts.voice.keep"), tr("setupFlow.tts.voice.keepDetail")},
				{"en-US-AriaNeural", tr("setupFlow.tts.voice.aria"), "en-US-AriaNeural"},
				{"zh-CN-XiaoxiaoNeural", tr("setupFlow.tts.voice.xiaoxiao"), "zh-CN-XiaoxiaoNeural"},
				{"zh-TW-HsiaoChenNeural", tr("setupFlow.tts.voice.hsiaochen"), "zh-TW-HsiaoChenNeural"},
				{"zh-HK-HiuMaanNeural", tr("setupFlow.tts.voice.hiumaan"), "zh-HK-HiuMaanNeural"},
				{"other", tr("setupFlow.tts.voice.other"), tr("setupFlow.tts.voice.otherDetail")}},
			settle: func(_ *sectionRun, value any) any {
				if value == "other" {
					return nil
				}
				return value
			}},
		sectionField{id: "edge_other", kind: fieldText, label: tr("setupFlow.tts.voiceOther"), targets: voice, when: answerIs("edge_voice", "other")})
	return sectionSpec{key: "tts", title: tr("setupFlow.tts.title"), detail: tr("setupFlow.tts.detail"), intro: tr("setupFlow.tts.intro"),
		facts: facts, fields: fields, fullSave: true}
}
