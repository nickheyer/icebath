package config

import (
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

func Register(cfg *config.Config) {
	runtimes := []config.Option{{Value: "", Title: "Image", About: "Start from the Vercel image setting"}}
	for _, runtime := range Runtimes {
		runtimes = append(runtimes, config.Option{Value: runtime, Title: runtime, About: "Vercel's " + runtime + " runtime, in place of the image"})
	}
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: terminalconfig.Key,
		Title:  "Vercel Sandbox",
		About:  "Commands run in a Vercel cloud microVM whose files are kept in snapshots",
		Settings: []config.Setting{
			{Key: Image, Title: "Vercel image", About: "Image Vercel sandboxes start from: a Vercel managed image or a VCR repository[:tag]", Default: DefaultImage, Env: []string{"TERMINAL_VERCEL_IMAGE"}},
			{Key: Runtime, Title: "Vercel runtime", About: "Language runtime for Vercel sandboxes, deprecated by Vercel and used in place of the image when set; empty uses the image", Default: "", Env: []string{"TERMINAL_VERCEL_RUNTIME"}, Options: config.Options(runtimes...)},
			{Key: Token, Title: "Vercel access token", About: "Long-lived Vercel access token, set together with the project and team IDs", Default: "", Env: []string{"VERCEL_TOKEN"}, Secret: true},
			{Key: ProjectID, Title: "Vercel project ID", About: "Project the access token makes sandboxes in, the projectId of .vercel/project.json", Default: "", Env: []string{"VERCEL_PROJECT_ID"}},
			{Key: TeamID, Title: "Vercel team ID", About: "Team that owns the project, the orgId of .vercel/project.json", Default: "", Env: []string{"VERCEL_TEAM_ID"}},
			{Key: OIDCToken, Title: "Vercel OIDC token", About: "Development-only token naming its project and team, used in place of the access token for one-off local work", Default: "", Env: []string{"VERCEL_OIDC_TOKEN"}, Secret: true},
		},
	}})
}
