package tool

import (
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

const (
	defaultResultChars    = 100_000
	defaultTurnChars      = 200_000
	defaultPreviewChars   = 1_500
	defaultMCPResultChars = 50_000

	perResultWindowShare   = 0.15
	perTurnWindowShare     = 0.30
	minResultChars         = 8_000
	minTurnChars           = 16_000
	mcpBudgetPrefix        = "mcp_"
	spilloverMinRetention  = 24 * time.Hour
	defaultSessionRetainDy = 90
)

type Budget struct {
	ResultChars  int
	TurnChars    int
	PreviewChars int
	MCPChars     int
}

func DefaultBudget() Budget {
	return Budget{ResultChars: defaultResultChars, TurnChars: defaultTurnChars, PreviewChars: defaultPreviewChars, MCPChars: defaultMCPResultChars}
}

func mcpResultChars(cfg *config.Config) int {
	if cfg != nil && cfg.ToolBudget.MCPResultSizeChars > 0 {
		return cfg.ToolBudget.MCPResultSizeChars
	}
	return defaultMCPResultChars
}

func BudgetFor(contextLength int, cfg *config.Config) Budget {
	b := DefaultBudget()
	b.MCPChars = mcpResultChars(cfg)
	if contextLength <= 0 {
		return b
	}
	window := float64(contextLength * charsPerToken)
	b.ResultChars = max(minResultChars, min(int(window*perResultWindowShare), defaultResultChars))
	b.TurnChars = max(minTurnChars, min(int(window*perTurnWindowShare), defaultTurnChars))
	return b
}

func (b Budget) Threshold(r *Registry, at Scope, tool string) int {
	if tool == "read_file" {
		return Unlimited
	}
	if strings.HasPrefix(tool, mcpBudgetPrefix) {
		return min(b.MCPChars, b.ResultChars)
	}
	if r != nil {
		if e, ok := r.Lookup(tool, at); ok {
			switch v := e.Tool.MaxResultChars; v {
			case 0:
			case Unlimited:
				return Unlimited
			default:
				return min(v, b.ResultChars)
			}
		}
	}
	return b.ResultChars
}

func SpillRetention(cfg *config.Config) time.Duration {
	days := defaultSessionRetainDy
	if cfg != nil {
		days = cfg.Sessions.RetentionDays
	}
	return max(spilloverMinRetention, time.Duration(days)*24*time.Hour)
}
