package gatewayrun

import (
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Voice reply modes a chat may hold
var voiceModeValues = map[string]bool{"off": true, "voice_only": true, "all": true}

// Persisted per chat voice modes, legacy unprefixed keys warned about and dropped, empty for a missing or bad file
func (r *GatewayRunner) loadVoiceModes() (*pydict.Dict[string, string], error) {
	raw, err := os.ReadFile(r.voiceModePath())
	if err != nil {
		return pydict.New[string, string](), nil
	}
	text, err := pystr.DecodeUTF8SigFile(raw)
	if err != nil {
		return nil, err
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return pydict.New[string, string](), nil
	}
	data, ok := parsed.(*pyjson.Object)
	if !ok {
		return pydict.New[string, string](), nil
	}
	keys := []string{}
	items := map[string]string{}
	for _, k := range data.Keys {
		v, _ := data.Get(k)
		switch v.(type) {
		case []any, *pyjson.Object:
			return nil, &pyTypeError{msg: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(v))}
		}
		m, isStr := v.(string)
		if !isStr || !voiceModeValues[m] {
			continue
		}
		if _, seen := items[k]; !seen {
			keys = append(keys, k)
		}
		items[k] = m
	}
	out := pydict.New[string, string]()
	for _, k := range keys {
		if !strings.Contains(k, ":") {
			runLog().Warn(fmt.Sprintf("Skipping legacy unprefixed voice mode key %s during migration. "+
				"Re-enable voice mode on that chat to rebuild the prefixed key.", pyrepr.Repr(k)))
			continue
		}
		out.Set(k, items[k])
	}
	return out, nil
}
