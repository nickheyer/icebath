package tui

import (
	"reflect"
	"strings"
	"testing"
)

// Load of the speech section over held config values, env values and gateway facts
func ttsLoad(held, env map[string]any, facts map[string]bool) map[string]any {
	answered := map[string]any{}
	for name, holds := range facts {
		answered[name] = holds
	}
	return map[string]any{"config": held, "env": env, "secrets": []any{}, "facts": answered, "values": map[string]any{}}
}

// Facts of a host with both engines supported, none installed and espeak-ng present
func ttsHost(extra map[string]bool) map[string]bool {
	facts := map[string]bool{"extra:neutts": true, "extra:kittentts": true, "binary:espeak-ng": true}
	for k, v := range extra {
		facts[k] = v
	}
	return facts
}

// Field of a spec by id
func specField(t *testing.T, spec sectionSpec, id string) sectionField {
	t.Helper()
	for _, f := range spec.fields {
		if f.id == id {
			return f
		}
	}
	t.Fatalf("no field %s", id)
	return sectionField{}
}

func TestTTSSectionKeepSavesTheWholeConfig(t *testing.T) {
	held := map[string]any{"tts.provider": "edge", "tts.edge.voice": "en-US-AriaNeural"}
	g, _ := walkSection(t, ttsSection(), ttsLoad(held, map[string]any{}, ttsHost(nil)), "Keep current")
	if g.saved["full_save"] != true || len(savedWrites(g)) != 0 {
		t.Fatalf("save %+v", g.saved)
	}
}

func TestTTSSectionEdgeWritesThePickedVoice(t *testing.T) {
	held := map[string]any{"tts.provider": "edge", "tts.edge.voice": "en-US-AriaNeural"}
	g, _ := walkSection(t, ttsSection(), ttsLoad(held, map[string]any{}, ttsHost(nil)), "Edge TTS", "Xiaoxiao")
	want := [][3]any{{"config", "tts.provider", "edge"}, {"config", "tts.edge.voice", "zh-CN-XiaoxiaoNeural"}}
	if got := savedWrites(g); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes %+v", got)
	}
	g, _ = walkSection(t, ttsSection(), ttsLoad(held, map[string]any{}, ttsHost(nil)), "Edge TTS", "Other voice", "de-DE-KatjaNeural")
	if got := savedWrites(g); !reflect.DeepEqual(got[1], [3]any{"config", "tts.edge.voice", "de-DE-KatjaNeural"}) {
		t.Fatalf("writes %+v", got)
	}
}

func TestTTSSectionKeyedProviderSavesTheKeyOrFallsBackToEdge(t *testing.T) {
	g, _ := walkSection(t, ttsSection(), ttsLoad(map[string]any{}, map[string]any{}, ttsHost(nil)), "ElevenLabs", "el-key")
	want := [][3]any{{"config", "tts.provider", "elevenlabs"}, {"env", "ELEVENLABS_API_KEY", "el-key"}}
	if got := savedWrites(g); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes %+v", got)
	}
	g, shown := walkSection(t, ttsSection(), ttsLoad(map[string]any{}, map[string]any{}, ttsHost(nil)), "ElevenLabs", "", "HiuMaan")
	want = [][3]any{{"config", "tts.provider", "edge"}, {"config", "tts.edge.voice", "zh-HK-HiuMaanNeural"}}
	if got := savedWrites(g); !reflect.DeepEqual(got, want) || !strings.Contains(shown, "falling back to Edge TTS") {
		t.Fatalf("writes %+v transcript\n%s", got, shown)
	}
	g, _ = walkSection(t, ttsSection(), ttsLoad(map[string]any{}, map[string]any{}, ttsHost(map[string]bool{"env:OPENAI_API_KEY": true})), "OpenAI TTS")
	if got := savedWrites(g); !reflect.DeepEqual(got, [][3]any{{"config", "tts.provider", "openai"}}) {
		t.Fatalf("a held key was asked again %+v", got)
	}
}

func TestTTSSectionXAILoginOutcomeDecidesTheProvider(t *testing.T) {
	load := ttsLoad(map[string]any{"tts.xai.voice_id": "ara"}, map[string]any{}, ttsHost(nil))
	g, _ := walkSection(t, ttsSection(), load, "xAI TTS", "Sign in with Grok OAuth", "")
	if got := savedWrites(g); !reflect.DeepEqual(got, [][3]any{{"config", "tts.provider", "xai"}}) || g.actions[0]["action"] != "xai.oauth.login" {
		t.Fatalf("writes %+v actions %+v", got, g.actions)
	}
	g, _ = walkSectionWith(t, &sectionGateway{load: load, failing: map[string]bool{"xai.oauth.login": true}}, ttsSection(), "xAI TTS", "Sign in with Grok OAuth", "Aria")
	if got := savedWrites(g); !reflect.DeepEqual(got, [][3]any{{"config", "tts.provider", "edge"}, {"config", "tts.edge.voice", "en-US-AriaNeural"}}) {
		t.Fatalf("writes %+v", got)
	}
	g, _ = walkSection(t, ttsSection(), load, "xAI TTS", "Paste an xAI API key", "xai-key", "eve-2")
	want := [][3]any{{"config", "tts.provider", "xai"}, {"env", "XAI_API_KEY", "xai-key"}, {"config", "tts.xai.voice_id", "eve-2"}}
	if got := savedWrites(g); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes %+v", got)
	}
	g, _ = walkSection(t, ttsSection(), ttsLoad(map[string]any{}, map[string]any{}, ttsHost(map[string]bool{"xai.oauth": true})), "xAI TTS", "")
	if got := savedWrites(g); !reflect.DeepEqual(got, [][3]any{{"config", "tts.provider", "xai"}}) {
		t.Fatalf("held login asked again %+v", got)
	}
}

func TestTTSSectionLocalEngineInstallsOrFallsBack(t *testing.T) {
	g, _ := walkSection(t, ttsSection(), ttsLoad(map[string]any{}, map[string]any{}, ttsHost(nil)), "NeuTTS", "Install now")
	if got := savedWrites(g); !reflect.DeepEqual(got, [][3]any{{"config", "tts.provider", "neutts"}}) || g.actions[0]["action"] != "tts.install.neutts" {
		t.Fatalf("writes %+v actions %+v", got, g.actions)
	}
	failing := &sectionGateway{load: ttsLoad(map[string]any{}, map[string]any{}, ttsHost(nil)), failing: map[string]bool{"tts.install.kittentts": true}}
	g, shown := walkSectionWith(t, failing, ttsSection(), "KittenTTS", "Install now", "Aria")
	if got := savedWrites(g); got[0] != [3]any{"config", "tts.provider", "edge"} || !strings.Contains(shown, "KittenTTS installation incomplete") {
		t.Fatalf("writes %+v transcript\n%s", got, shown)
	}
	g, shown = walkSection(t, ttsSection(), ttsLoad(map[string]any{}, map[string]any{}, ttsHost(map[string]bool{"extra:kittentts": false})), "KittenTTS")
	if got := savedWrites(g); !reflect.DeepEqual(got, [][3]any{{"config", "tts.provider", "kittentts"}}) || !strings.Contains(shown, "not supported on this platform") {
		t.Fatalf("writes %+v transcript\n%s", got, shown)
	}
	g, shown = walkSection(t, ttsSection(), ttsLoad(map[string]any{}, map[string]any{}, ttsHost(map[string]bool{"module:neutts": true})), "NeuTTS")
	if got := savedWrites(g); !reflect.DeepEqual(got, [][3]any{{"config", "tts.provider", "neutts"}}) || !strings.Contains(shown, "NeuTTS is already installed") {
		t.Fatalf("writes %+v transcript\n%s", got, shown)
	}
}

func TestTTSSectionEspeakFailureStopsTheNeuTTSInstall(t *testing.T) {
	spec := ttsSection()
	run := &sectionRun{spec: spec, facts: map[string]bool{"extra:neutts": true}, answers: map[string]any{"provider": "neutts", "install_neutts": true}}
	for _, id := range []string{"espeak_needed", "espeak"} {
		if !specField(t, spec, id).when(run) {
			t.Fatalf("%s not asked without espeak", id)
		}
	}
	run.answers["espeak"], run.answers["espeak_run"] = true, false
	if specField(t, spec, "sync_neutts").when(run) || !specField(t, spec, "neutts_failed").when(run) || ttsOutcome(run) != "edge" {
		t.Fatal("a failed espeak install still ran the engine install")
	}
	run.answers["espeak_run"] = true
	if !specField(t, spec, "sync_neutts").when(run) || !specField(t, spec, "espeak_done").when(run) {
		t.Fatal("an installed espeak did not lead to the engine install")
	}
	run.answers["espeak"] = false
	delete(run.answers, "espeak_run")
	if !specField(t, spec, "espeak_skipped").when(run) || !specField(t, spec, "sync_neutts").when(run) {
		t.Fatal("a declined espeak install must warn and still install the engine")
	}
}

func TestTTSSectionNousRouteUsesManagedOpenAI(t *testing.T) {
	facts := ttsHost(map[string]bool{"nous.managed_tools": true, "env:OPENAI_API_KEY": true})
	g, shown := walkSection(t, ttsSection(), ttsLoad(map[string]any{}, map[string]any{}, facts), "Nous Subscription")
	if got := savedWrites(g); !reflect.DeepEqual(got, [][3]any{{"config", "tts.provider", "openai"}}) {
		t.Fatalf("writes %+v", got)
	}
	if !strings.Contains(shown, "managed Nous gateway") || !strings.Contains(shown, "Direct OpenAI keys are still in .env") {
		t.Fatalf("transcript\n%s", shown)
	}
}

func TestAgentSectionSavesAtTheWizardSavePoint(t *testing.T) {
	g, _ := walkSection(t, agentSection(), sectionLoad(map[string]any{}, map[string]any{}, []any{}, map[string]any{}), "120", "new", "0.7")
	if len(g.saves) != 2 || g.saves[0]["full_save"] != true || g.saves[1]["full_save"] != true {
		t.Fatalf("saves %+v", g.saves)
	}
	first := savedWritesOf(g.saves[0])
	want := [][3]any{{"config", "compression.enabled", true}, {"config", "agent.max_turns", 120}, {"config", "display.tool_progress", "new"}}
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(savedWritesOf(g.saves[1]), [][3]any{{"config", "compression.threshold", 0.7}}) {
		t.Fatalf("batches %+v then %+v", first, savedWritesOf(g.saves[1]))
	}
	g, _ = walkSection(t, agentSection(), sectionLoad(map[string]any{}, map[string]any{}, []any{}, map[string]any{}), "", "keep", "")
	if len(g.saves) != 1 || !reflect.DeepEqual(savedWritesOf(g.saves[0]), [][3]any{{"config", "compression.enabled", true}, {"config", "compression.threshold", 0.5}}) {
		t.Fatalf("keep walk saves %+v", g.saves)
	}
}

// Writes of one save as target, key and value triples in order
func savedWritesOf(save map[string]any) [][3]any {
	var out [][3]any
	for _, raw := range save["writes"].([]any) {
		w := raw.(map[string]any)
		out = append(out, [3]any{w["target"], w["key"], w["value"]})
	}
	return out
}
