package approval

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tool"
)

type ModeResult struct {
	OK      bool
	Mode    config.ApprovalMode
	Changed bool
	Message string
}

func SetMode(ctx context.Context, st *config.Store, o tool.Origin, requested string) ModeResult {
	current := Mode(ctx, st, o)
	want := config.ApprovalMode(pyLower(pyStrip(requested)))
	if want == "" {
		return ModeResult{OK: true, Mode: current, Message: "Approval mode: " + string(current) + " (persistent profile setting)."}
	}
	switch want {
	case config.ApprovalManual, config.ApprovalSmart, config.ApprovalOff:
	default:
		return ModeResult{Mode: current, Message: "Usage: /approvals [manual|smart|off]"}
	}
	if _, err := st.SetValue(ctx, "approvals.mode", string(want), false); err != nil {
		var managed *config.ManagedKeyError
		if errors.As(err, &managed) {
			detail := pyStrip(managed.Error())
			if detail == "" {
				detail = "Approval mode is managed and cannot be changed."
			}
			return ModeResult{Mode: current, Message: detail}
		}
		return ModeResult{Mode: current, Message: "Failed to save approval mode: " + err.Error()}
	}
	effective := Mode(ctx, st, o)
	if effective != want {
		return ModeResult{Mode: effective, Message: "Approval mode remains " + string(effective) + "; the requested value did not become effective."}
	}
	return ModeResult{OK: true, Mode: effective, Changed: effective != current, Message: "Approval mode: " + string(effective) + " (persistent profile setting)."}
}
