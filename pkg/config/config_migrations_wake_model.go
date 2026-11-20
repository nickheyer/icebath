package config

import "strings"

// Model names that selected the wake model icebath no longer ships
var retiredWakeModels = map[string]bool{"hey_hermes": true, "hey hermes": true, "hermes": true}

// Provider aliases that run the openWakeWord engine
var openWakeWordProviders = map[string]bool{"openwakeword": true, "oww": true, "local": true}

// Lowercase words of a wake phrase or model name, underscores read as spaces
func wakeWords(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(s), "_", " ")), " ")
}

// Moves a config off the wake model icebath no longer ships, to the icebath phrase on the open vocabulary engine
func migrateTo65(r *migrationRun) error {
	cfg := r.read()
	wake, ok := cfg["wake_word"].(map[string]any)
	if !ok {
		return nil
	}
	var added, notes []string
	if phrase, ok := wake["phrase"].(string); ok && wakeWords(phrase) == retiredWakePhrase {
		wake["phrase"] = "hey icebath"
		added = append(added, "wake_word.phrase=hey icebath")
		notes = append(notes, "  ✓ Changed wake_word.phrase to \"hey icebath\". The model that detected the earlier phrase is no longer shipped, "+
			"and the open vocabulary sherpa engine detects \"hey icebath\".")
	}
	oww, _ := wake["openwakeword"].(map[string]any)
	model, hasModel := oww["model"].(string)
	if hasModel && retiredWakeModels[wakeWords(model)] {
		delete(oww, "model")
		if len(oww) == 0 {
			delete(wake, "openwakeword")
		}
		hasModel = false
		added = append(added, "removed wake_word.openwakeword.model")
		notes = append(notes, "  ✓ Removed wake_word.openwakeword.model, it named the wake model icebath no longer ships.")
	}
	provider, _ := wake["provider"].(string)
	if openWakeWordProviders[strings.ToLower(strings.TrimSpace(provider))] && (!hasModel || strings.TrimSpace(model) == "") {
		wake["provider"] = "auto"
		added = append(added, "wake_word.provider=auto (was: "+provider+")")
		notes = append(notes, "  ✓ Changed wake_word.provider from "+provider+" to auto. openWakeWord ran on the wake model icebath no longer ships "+
			"and has no model file configured, so the open vocabulary sherpa engine detects the wake phrase now. Set wake_word.openwakeword.model "+
			"to a model file and wake_word.provider back to openwakeword to use it.")
	}
	if len(added) == 0 {
		return nil
	}
	cfg["wake_word"] = wake
	return r.commit(cfg, strings.Join(added, ", "), strings.Join(notes, "\n"))
}
