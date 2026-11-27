package auxiliary

import (
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	visionconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/vision/config"
)

type tasks struct{}

func Register(registry *auxiliary.Registry) {
	registry.Register(tasks{})
}

func (tasks) Tasks() []auxiliary.Task {
	return []auxiliary.Task{
		{Name: visionconfig.Task, Title: "Image analysis", About: "The model that looks at images for the agent", Order: 30, Timeout: 120, Vision: true,
			Critical: true, Settings: []config.Setting{
				{Key: visionconfig.Temperature, Title: "Temperature", About: "How much the descriptions of images and videos may vary.",
					Help: "Low values give steadier descriptions.", Default: 0.1, Least: config.Num(0), Most: config.Num(2), Advanced: true},
				{Key: visionconfig.DownloadTimeout, Title: "Download time limit", About: "How long a download of an image or video may go without data.",
					Default: 30 * time.Second, Zero: "1 second", Advanced: true},
				{Key: visionconfig.MaxConcurrency, Title: "Images at once", About: "How many images are prepared at the same time.", Default: 0,
					Zero: "One per core", Counts: "images", Least: config.Num(0), Advanced: true},
			}},
		{Name: visionconfig.VideoTask, Title: "Video analysis", About: "The model that watches videos for the agent", Order: 40, Inherit: visionconfig.Task,
			MinTimeout: 180, Vision: true, Critical: true},
	}
}
