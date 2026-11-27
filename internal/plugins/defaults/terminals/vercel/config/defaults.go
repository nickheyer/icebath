package config

import (
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

// Names of the runtimes as people know them, the runtime's own name for one not here
var runtimeTitles = map[string]string{"node24": "Node.js 24", "node22": "Node.js 22", "python3.13": "Python 3.13"}

func Register(cfg *config.Config) {
	runtimes := []config.Option{{Value: "", Title: "Use the image", About: "Sandboxes start from the Image setting"}}
	for _, runtime := range Runtimes {
		title := runtimeTitles[runtime]
		if title == "" {
			title = runtime
		}
		runtimes = append(runtimes, config.Option{Value: runtime, Title: title, About: "Vercel's " + title + " runtime, in place of the image"})
	}
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: terminalconfig.Key,
		Title:  "Vercel Sandbox",
		About:  "A Vercel cloud sandbox whose files are kept in snapshots",
		Order:  60,
		Settings: []config.Setting{
			{Key: Token, Title: "Access token", About: "Your Vercel access token.", Help: "Set the project ID and the team ID as well.", Default: "",
				Env: []string{"VERCEL_TOKEN"}, Secret: true},
			{Key: ProjectID, Title: "Project ID", About: "The Vercel project that sandboxes are made in.", Help: "The projectId in .vercel/project.json.",
				Default: "", Env: []string{"VERCEL_PROJECT_ID"}},
			{Key: TeamID, Title: "Team ID", About: "The Vercel team that owns the project.", Help: "The orgId in .vercel/project.json.", Default: "",
				Env: []string{"VERCEL_TEAM_ID"}},
			{Key: Image, Title: "Image", About: "The image that Vercel sandboxes start from.", Help: "An image that Vercel manages, or a VCR repository[:tag].",
				Default: DefaultImage, Env: []string{"TERMINAL_VERCEL_IMAGE"}},
			{Key: Runtime, Title: "Runtime", About: "Which language runtime sandboxes start from in place of the image.",
				Help: "Vercel no longer recommends runtimes.", Default: "", Env: []string{"TERMINAL_VERCEL_RUNTIME"}, Options: config.Options(runtimes...),
				Advanced: true},
			{Key: OIDCToken, Title: "OIDC token", About: "Your Vercel OIDC token, for work on this computer only.",
				Help: "Used in place of the access token. The token names its own project and team.", Default: "", Env: []string{"VERCEL_OIDC_TOKEN"},
				Secret: true, Advanced: true},
		},
	}})
}
