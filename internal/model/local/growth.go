package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

type windowOverride struct {
	model  string
	window int
}

func (r *Runtime) WindowOverridesPath() string { return home.LlamaCppWindowOverrides.In(r.root) }

func overrideValue(v any) (int, bool) {
	switch t := v.(type) {
	case json.Number:
		if n, err := strconv.Atoi(string(t)); err == nil {
			return n, true
		}
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil {
			return 0, false
		}
		return int(f), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func (r *Runtime) loadOverrides() []windowOverride {
	data, err := os.ReadFile(r.WindowOverridesPath())
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var out []windowOverride
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		var raw any
		if err := dec.Decode(&raw); err != nil {
			return nil
		}
		window, ok := overrideValue(raw)
		if !ok {
			return nil
		}
		key := keyTok.(string)
		replaced := false
		for i := range out {
			if out[i].model == key {
				out[i].window, replaced = window, true
			}
		}
		if !replaced {
			out = append(out, windowOverride{key, window})
		}
	}
	return out
}

func (r *Runtime) LoadWindowOverrides() map[string]int {
	out := map[string]int{}
	for _, o := range r.loadOverrides() {
		out[o.model] = o.window
	}
	return out
}

func (r *Runtime) writeOverrides(ctx context.Context, overrides []windowOverride) error {
	path := r.WindowOverridesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	var b strings.Builder
	if len(overrides) == 0 {
		b.WriteString("{}")
	} else {
		b.WriteString("{\n")
		for i, o := range overrides {
			key, err := fileutil.EncodeJSON(o.model, fileutil.JSONStyle{ASCII: true})
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %s: %d", strings.TrimRight(string(key), "\n"), o.window)
			if i < len(overrides)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString("}")
	}
	return fileutil.WriteFile(ctx, path, []byte(b.String()))
}

func (r *Runtime) SaveWindowOverride(ctx context.Context, modelID string, window int) error {
	overrides := r.loadOverrides()
	for i := range overrides {
		if overrides[i].model == modelID {
			overrides[i].window = window
			return r.writeOverrides(ctx, overrides)
		}
	}
	return r.writeOverrides(ctx, append(overrides, windowOverride{modelID, window}))
}

func (r *Runtime) ClearWindowOverride(ctx context.Context, modelID string) error {
	overrides := r.loadOverrides()
	for i := range overrides {
		if overrides[i].model == modelID {
			return r.writeOverrides(ctx, append(overrides[:i:i], overrides[i+1:]...))
		}
	}
	return nil
}

func (r *Runtime) IsManagedEndpoint(baseURL string) bool {
	state := r.stateEndpoint()
	return state != nil && strings.TrimRight(baseURL, "/") == strings.TrimRight(state.BaseURL, "/")
}

type GrowthRequest struct {
	ModelID            string
	BaseURL            string
	SessionTokens      int
	CurrentWindow      int
	MeasuredDecodeTokS *float64
}

func (r *Runtime) MaybeGrowWindow(ctx context.Context, q GrowthRequest) (int, error) {
	sup := r.Supervisor()
	if sup == nil || !r.IsManagedEndpoint(q.BaseURL) {
		return 0, nil
	}
	var gguf string
	for _, p := range r.StagedModels() {
		if ModelIDFromStem(strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))) == q.ModelID {
			gguf = p
			break
		}
	}
	if gguf == "" {
		return 0, nil
	}
	header, err := ReadGGUFHeader(gguf)
	if err != nil {
		slog.Debug(fmt.Sprintf("growth skip %s: unreadable gguf (%v)", q.ModelID, err))
		return 0, nil
	}
	profile := ProfileFromGGUF(header)
	serverIdle := sup.IsIdle(ctx, q.ModelID)
	budget := r.ProbeBudget(ctx, true)
	decision := DecideGrowth(AsLoaded(profile, budget), budget, GrowthInput{
		CurrentWindow: q.CurrentWindow, SessionTokens: q.SessionTokens, MeasuredDecodeTokS: q.MeasuredDecodeTokS,
		ServerIdle: serverIdle, FlashAttention: true, OccupancyConfirmed: true,
	})
	if decision.Action != Grow || decision.NextWindow == 0 {
		slog.Debug(fmt.Sprintf("growth %s: %s (%s)", q.ModelID, decision.Action, decision.Reason))
		return 0, nil
	}
	running, ok := r.ReadPresetDecisions("")[q.ModelID]
	drafting := false
	if ok {
		spec, _ := running.Key(presetKeySpecType)
		drafting = spec == dflashSpecType
	}
	own, _ := r.residentFootprint(gguf, budget, q.CurrentWindow, drafting)
	live := r.LaunchBudget(ctx, budget, own)
	plan := r.presetForModel(gguf, budget, presetRequest{requestedWindow: decision.NextWindow, live: live, residents: 1})
	if plan == nil || plan.Refusal != "" || plan.Window < decision.NextWindow {
		slog.Debug(fmt.Sprintf("growth %s: the next rung does not fit beside other programs' GPU memory", q.ModelID))
		return 0, nil
	}
	slog.Info(fmt.Sprintf("context growth %s: %s", q.ModelID, decision.Reason))
	if err := r.SaveWindowOverride(ctx, q.ModelID, decision.NextWindow); err != nil {
		return 0, err
	}
	if !r.Refresh(ctx) {
		slog.Warn(fmt.Sprintf("growth %s: server refresh failed; compression proceeds", q.ModelID))
		return 0, nil
	}
	materialized, ok := r.ReadPresetDecisions("")[q.ModelID]
	if !ok || materialized.Window < decision.NextWindow {
		slog.Warn(fmt.Sprintf("growth %s: refreshed preset did not grant the requested window", q.ModelID))
		return 0, nil
	}
	return materialized.Window, nil
}
