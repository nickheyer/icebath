package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// CompressionSettings is the parsed compression config section
type CompressionSettings struct {
	Threshold              float64
	AutoraiseNoticeEnabled bool
	Enabled                bool
}

// Float form of an autoraise field, the KeyError or ValueError of a missing or non numeric one
func autoraiseFloat(autoraise map[string]any, key string) (float64, error) {
	raw, ok := autoraise[key]
	if !ok {
		return 0, fmt.Errorf("KeyError: %s", pyrepr.Repr(key))
	}
	switch v := raw.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, fmt.Errorf("ValueError: could not convert string to float: %s", pyrepr.Repr(v))
		}
		return f, nil
	}
	return 0, fmt.Errorf("TypeError: float() argument must be a string or a real number")
}

func autoraiseModel(autoraise map[string]any, fallback string) string {
	model := fallback
	if pyrepr.Truthy(autoraise["model"]) {
		model = pyrepr.Str(autoraise["model"])
	}
	model = strings.ToLower(strings.TrimSpace(model))
	return model[strings.LastIndex(model, "/")+1:]
}

// One time notice that Codex gpt-5.x raised compaction, reporting the live window and the opt out command
func buildCodexGPT5AutoraiseNotice(autoraise map[string]any, contextLength int) string {
	model := autoraiseModel(autoraise, "gpt-5.4/5.5")
	limit := "272K"
	if contextLength > 0 {
		limit = fmt.Sprintf("%dK", int(math.RoundToEven(float64(contextLength)/1000)))
	} else if strings.HasPrefix(model, "gpt-5.3-codex-spark") {
		limit = "128K"
	}
	from, err := autoraiseFloat(autoraise, "from")
	if err != nil {
		panic(err)
	}
	to, err := autoraiseFloat(autoraise, "to")
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("ℹ Codex %s caps context at %s, so auto-compaction was raised to %d%% (from %d%%) to use more of the window before summarizing.\n"+
		"  Opt back out: icebath config set compression.codex_gpt55_autoraise false",
		model, limit, int(math.RoundToEven(to*100)), int(math.RoundToEven(from*100)))
}

func codexGPT55AutoraiseNoticeMarker(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), ".codex_gpt55_autoraise_notice")
}

// Notice identity keyed on the model and the from and to percentages it displays
func codexGPT55AutoraiseNoticeState(autoraise map[string]any) (string, error) {
	from, err := autoraiseFloat(autoraise, "from")
	if err != nil {
		return "", err
	}
	to, err := autoraiseFloat(autoraise, "to")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d:%d", autoraiseModel(autoraise, ""), int(math.RoundToEven(from*100)), int(math.RoundToEven(to*100))), nil
}

// True when this exact notice was already shown for the profile, an unreadable marker reading unseen
func codexGPT55AutoraiseNoticeSeen(ctx context.Context, autoraise map[string]any) bool {
	current, err := codexGPT55AutoraiseNoticeState(autoraise)
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(codexGPT55AutoraiseNoticeMarker(ctx))
	return err == nil && strings.TrimSpace(string(raw)) == current
}

// Persists that the notice was shown, a failure only showing it again later
func recordCodexGPT55AutoraiseNotice(ctx context.Context, autoraise map[string]any) {
	current, err := codexGPT55AutoraiseNoticeState(autoraise)
	if err != nil {
		return
	}
	marker := codexGPT55AutoraiseNoticeMarker(ctx)
	if os.MkdirAll(filepath.Dir(marker), 0o755) != nil {
		return
	}
	_ = os.WriteFile(marker, []byte(current), 0o644)
}

// Prints the context limit banner, stages the once per profile autoraise notice and rearms feasibility
func emitCompressionSummary(ctx context.Context, a *AIAgent, cs CompressionSettings) {
	autoraise := a.compressionThresholdAutoraised
	notice := ""
	cc := a.contextCompressor
	if len(autoraise) > 0 && cs.Enabled && cs.AutoraiseNoticeEnabled && !codexGPT55AutoraiseNoticeSeen(ctx, autoraise) {
		notice = buildCodexGPT5AutoraiseNotice(autoraise, cc.Base().ContextLength)
	}
	if !a.quietMode {
		base := cc.Base()
		if cs.Enabled {
			capNote := ""
			if eff := engineEffectiveThresholdCap(cc, base.ContextLength); eff != nil && base.ThresholdTokens == *eff {
				capValue, _ := engineAttr(cc, "threshold_tokens_cap")
				capNote = fmt.Sprintf(" (capped at %s tokens)", formatThousandsAny(capValue))
			}
			_, _ = fmt.Fprintf(statusStdout, "\U0001f4ca Context limit: %s tokens (compress at %d%% = %s%s)\n",
				groupThousands(base.ContextLength), int(base.ThresholdPercent*100), groupThousands(base.ThresholdTokens), capNote)
		} else {
			_, _ = fmt.Fprintf(statusStdout, "\U0001f4ca Context limit: %s tokens (auto-compression disabled)\n", groupThousands(base.ContextLength))
		}
		if notice != "" {
			a.safePrint(notice, true)
		}
	}
	a.compressionWarning = nil
	if notice != "" {
		a.compressionWarning = notice
		recordCodexGPT55AutoraiseNotice(ctx, autoraise)
	}
	a.compressionFeasibilityChecked = false
}
