package tui

import (
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
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

// Key of the label that asks for the API key of each keyed speech provider
var ttsKeyLabels = map[string]txt.Key{
	"elevenlabs": txt.SetupFlowTtsKeyLabelElevenlabs,
	"openai":     txt.SetupFlowTtsKeyLabelOpenai,
	"minimax":    txt.SetupFlowTtsKeyLabelMinimax,
	"mistral":    txt.SetupFlowTtsKeyLabelMistral,
	"gemini":     txt.SetupFlowTtsKeyLabelGemini,
}

// Speech provider picked in this walk, empty when the current one is kept
func ttsPicked(run *sectionRun) string {
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

// Speech provider menu, the current one kept first
func ttsProviders() []sectionChoice {
	return []sectionChoice{{nil, tr(txt.SetupFlowTtsKeep), tr(txt.SetupFlowTtsKeepDetail)},
		sectionChoice{"edge", "Edge TTS", tr(txt.SetupFlowTtsProviderEdge)},
		sectionChoice{"elevenlabs", "ElevenLabs", tr(txt.SetupFlowTtsProviderElevenlabs)},
		sectionChoice{"openai", "OpenAI TTS", tr(txt.SetupFlowTtsProviderOpenai)},
		sectionChoice{"xai", "xAI TTS", tr(txt.SetupFlowTtsProviderXai)},
		sectionChoice{"minimax", "MiniMax TTS", tr(txt.SetupFlowTtsProviderMinimax)},
		sectionChoice{"mistral", "Mistral Voxtral TTS", tr(txt.SetupFlowTtsProviderMistral)},
		sectionChoice{"gemini", "Google Gemini TTS", tr(txt.SetupFlowTtsProviderGemini)},
		sectionChoice{"neutts", "NeuTTS", tr(txt.SetupFlowTtsProviderNeutts)},
		sectionChoice{"kittentts", "KittenTTS", tr(txt.SetupFlowTtsProviderKittentts)}}
}

// Questions of one local speech engine, its platform support, install offer, system package and install run, needs being the key of the notice of what it installs
func ttsEngineFields(engine, name string, needs txt.Key) []sectionField {
	picked, supported, present := ttsPickedIs(engine), factHolds("extra:"+engine), factHolds("module:"+engine)
	offered := allOf(picked, supported, notCond(present))
	install, sync := "install_"+engine, "sync_"+engine
	fields := []sectionField{
		{id: engine + "_unsupported", kind: fieldNotice, label: tr(txt.SetupFlowTtsUnsupported, name), when: allOf(picked, notCond(supported))},
		{id: engine + "_present", kind: fieldNotice, label: tr(txt.SetupFlowTtsInstalled, name), when: allOf(picked, supported, present)},
		{id: engine + "_needs", kind: fieldNotice, label: tr(needs), when: offered},
		{id: install, kind: fieldChoice, label: tr(txt.SetupFlowTtsInstallLabel), when: offered, choices: []sectionChoice{
			{true, tr(txt.SetupFlowTtsInstallYes), tr(txt.SetupFlowTtsInstallYesDetail)},
			{false, tr(txt.SetupFlowTtsInstallNo), tr(txt.SetupFlowTtsInstallNoDetail)}}},
		{id: engine + "_skipped", kind: fieldNotice, label: tr(txt.SetupFlowTtsInstallSkipped, engine), when: answerIs(install, false)},
	}
	ready := answerIs(install, true)
	if engine == "neutts" {
		argv := espeakInstall()
		missing := allOf(ready, notCond(factHolds("binary:espeak-ng")), notCond(factHolds("binary:espeak")))
		fields = append(fields,
			sectionField{id: "espeak_needed", kind: fieldNotice, label: tr(txt.SetupFlowTtsEspeakNeeded, strings.Join(argv, " ")), when: missing},
			sectionField{id: "espeak", kind: fieldChoice, label: tr(txt.SetupFlowTtsEspeakLabel), when: missing, choices: []sectionChoice{
				{true, tr(txt.SetupFlowTtsEspeakYes), tr(txt.SetupFlowTtsEspeakYesDetail)},
				{false, tr(txt.SetupFlowTtsEspeakNo), tr(txt.SetupFlowTtsEspeakNoDetail)}}},
			sectionField{id: "espeak_run", kind: fieldCommand, argv: argv, when: answerIs("espeak", true)},
			sectionField{id: "espeak_done", kind: fieldNotice, label: tr(txt.SetupFlowTtsEspeakDone), when: answerIs("espeak_run", true)},
			sectionField{id: "espeak_failed", kind: fieldNotice, label: tr(txt.SetupFlowTtsEspeakFailed), when: answerIs("espeak_run", false)},
			sectionField{id: "espeak_skipped", kind: fieldNotice, label: tr(txt.SetupFlowTtsEspeakSkipped), when: answerIs("espeak", false)})
		ready = allOf(ready, notCond(answerIs("espeak_run", false)))
	}
	return append(fields,
		sectionField{id: sync, kind: fieldAction, action: tuigateway.SetupAction("tts.install." + engine), label: tr(txt.SetupFlowTtsInstalling), when: ready},
		sectionField{id: engine + "_failed", kind: fieldNotice, label: tr(txt.SetupFlowTtsInstallFailed, name),
			when: allOf(answerIs(install, true), notCond(answerIs(sync, true)))})
}

// Speech provider with its key, login or local engine, then the voice of the provider the walk ends on
func ttsSection() sectionSpec {
	provider := []sectionTarget{{key: "tts.provider"}}
	settled := func(run *sectionRun, _ any) any { return ttsOutcome(run) }
	facts := []string{"xai.oauth", "env:XAI_API_KEY", "extra:neutts", "extra:kittentts", "module:neutts", "module:kittentts",
		"binary:espeak-ng", "binary:espeak"}
	fields := []sectionField{
		{id: "provider", kind: fieldChoice, label: tr(txt.SetupFlowTtsProviderLabel), targets: provider, fallback: "edge",
			choices: ttsProviders(), settle: settled},
	}
	for _, name := range ttsKeyed {
		for _, key := range ttsKeyEnvs[name] {
			facts = append(facts, "env:"+key)
		}
		asked := allOf(ttsPickedIs(name), notCond(ttsKeyHeld(name)))
		if name == "gemini" {
			fields = append(fields, sectionField{id: "gemini_hint", kind: fieldNotice, label: tr(txt.SetupFlowTtsGeminiHint), when: asked})
		}
		fields = append(fields,
			sectionField{id: "key_" + name, kind: fieldSecret, label: tr(ttsKeyLabels[name]),
				targets: []sectionTarget{{env: true, key: ttsKeyEnvs[name][0]}}, when: asked},
			sectionField{id: "no_key_" + name, kind: fieldNotice, label: tr(txt.SetupFlowTtsNoKey), when: allOf(asked, answerIs("key_"+name, nil))})
	}
	xai := ttsPickedIs("xai")
	xaiKey, xaiOAuth := factHolds("env:XAI_API_KEY"), factHolds("xai.oauth")
	fields = append(fields,
		sectionField{id: "xai_key_held", kind: fieldNotice, label: tr(txt.SetupFlowTtsXaiKeyHeld), when: allOf(xai, xaiKey)},
		sectionField{id: "xai_oauth_held", kind: fieldNotice, label: tr(txt.SetupFlowTtsXaiOAuthHeld), when: allOf(xai, notCond(xaiKey), xaiOAuth)},
		sectionField{id: "xai_auth", kind: fieldChoice, label: tr(txt.SetupFlowTtsXaiAuthLabel), when: allOf(xai, notCond(xaiKey), notCond(xaiOAuth)),
			choices: []sectionChoice{
				{"oauth", tr(txt.SetupFlowTtsXaiAuthOauth), tr(txt.SetupFlowTtsXaiAuthOauthDetail)},
				{"key", tr(txt.SetupFlowTtsXaiAuthKey), tr(txt.SetupFlowTtsXaiAuthKeyDetail)},
				{"skip", tr(txt.SetupFlowTtsXaiAuthSkip), tr(txt.SetupFlowTtsXaiAuthSkipDetail)}}},
		sectionField{id: "xai_login", kind: fieldAction, action: "xai.oauth.login", label: tr(txt.SetupFlowTtsXaiLogin), when: answerIs("xai_auth", "oauth")},
		sectionField{id: "xai_login_done", kind: fieldNotice, label: tr(txt.SetupFlowTtsXaiLoginDone), when: answerIs("xai_login", true)},
		sectionField{id: "xai_login_failed", kind: fieldNotice, label: tr(txt.SetupFlowTtsXaiLoginFailed), when: answerIs("xai_login", false)},
		sectionField{id: "xai_key", kind: fieldSecret, label: tr(txt.SetupFlowTtsXaiKeyLabel),
			targets: []sectionTarget{{env: true, key: "XAI_API_KEY"}}, when: answerIs("xai_auth", "key")},
		sectionField{id: "xai_no_key", kind: fieldNotice, label: tr(txt.SetupFlowTtsXaiNoKey), when: allOf(answerIs("xai_auth", "key"), answerIs("xai_key", nil))},
		sectionField{id: "xai_skipped", kind: fieldNotice, label: tr(txt.SetupFlowTtsXaiSkipped), when: answerIs("xai_auth", "skip")},
		sectionField{id: "xai_voice", kind: fieldText, label: tr(txt.SetupFlowTtsXaiVoice),
			targets: []sectionTarget{{key: "tts.xai.voice_id"}}, when: ttsOutcomeIs("xai"), seed: seedOf(nil)})
	fields = append(fields, ttsEngineFields("neutts", "NeuTTS", txt.SetupFlowTtsNeuttsNeeds)...)
	fields = append(fields, ttsEngineFields("kittentts", "KittenTTS", txt.SetupFlowTtsKittenttsNeeds)...)
	voice := []sectionTarget{{key: "tts.edge.voice"}}
	fields = append(fields,
		sectionField{id: "edge_voice", kind: fieldChoice, label: tr(txt.SetupFlowTtsVoiceLabel), targets: voice, fallback: "en-US-AriaNeural",
			when: ttsOutcomeIs("edge"), choices: []sectionChoice{
				{nil, tr(txt.SetupFlowTtsVoiceKeep), tr(txt.SetupFlowTtsVoiceKeepDetail)},
				{"en-US-AriaNeural", tr(txt.SetupFlowTtsVoiceAria), "en-US-AriaNeural"},
				{"zh-CN-XiaoxiaoNeural", tr(txt.SetupFlowTtsVoiceXiaoxiao), "zh-CN-XiaoxiaoNeural"},
				{"zh-TW-HsiaoChenNeural", tr(txt.SetupFlowTtsVoiceHsiaochen), "zh-TW-HsiaoChenNeural"},
				{"zh-HK-HiuMaanNeural", tr(txt.SetupFlowTtsVoiceHiumaan), "zh-HK-HiuMaanNeural"},
				{"other", tr(txt.SetupFlow_Tts_Voice_Other), tr(txt.SetupFlowTtsVoiceOtherDetail)}},
			settle: func(_ *sectionRun, value any) any {
				if value == "other" {
					return nil
				}
				return value
			}},
		sectionField{id: "edge_other", kind: fieldText, label: tr(txt.SetupFlow_Tts_VoiceOther), targets: voice, when: answerIs("edge_voice", "other")})
	return sectionSpec{key: "tts", title: tr(txt.SetupFlowTtsTitle), detail: tr(txt.SetupFlowTtsDetail), intro: tr(txt.SetupFlowTtsIntro),
		facts: facts, fields: fields, fullSave: true}
}
