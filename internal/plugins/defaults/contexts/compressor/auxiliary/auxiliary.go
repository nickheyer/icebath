package auxiliary

import (
	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/contexts/compressor/engine"
)

type tasks struct{}

func Register(registry *auxiliary.Registry) {
	registry.Register(tasks{})
}

func (tasks) Tasks() []auxiliary.Task {
	return []auxiliary.Task{{Name: engine.Task, Title: "Summaries", About: "The model that summarizes long sessions", Order: 20, Timeout: 120,
		Settings: []config.Setting{
			{Key: engine.AuxWindow, Title: "Context window", About: "How much text the summary model can read at once.",
				Help: "Set this only when icebath gets the size wrong.", Default: 0, Zero: "Automatic", Counts: "tokens", Least: config.Num(0), Advanced: true},
		}}}
}
