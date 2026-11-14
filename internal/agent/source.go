package agent

import "strings"

const (
	SourceCLI     = "cli"
	SourceTUI     = "tui"
	SourceDesktop = "desktop"
	SourceACP     = "acp"
	SourceOneshot = "oneshot"

	PlatformSubagent = "subagent"

	EnvSessionSource         = "ICEBATH_SESSION_SOURCE"
	EnvSingleQuerySession    = "ICEBATH_SINGLE_QUERY_SESSION"
	EnvSessionSourceExplicit = "ICEBATH_SESSION_SOURCE_EXPLICIT"
)

type SourceEnv struct {
	Source      string
	SingleQuery bool
	Explicit    bool
}

func SourceEnvFrom(getenv func(string) string) SourceEnv {
	return SourceEnv{
		Source:      strings.TrimFunc(getenv(EnvSessionSource), textSpace),
		SingleQuery: getenv(EnvSingleQuerySession) == "1",
		Explicit:    getenv(EnvSessionSourceExplicit) == "1",
	}
}

func UITransportSource(source string) bool {
	return source == SourceTUI || source == SourceDesktop
}

func CLIFamilySource(source string) bool {
	return source == SourceCLI || source == SourceOneshot
}

func DaybreakSource(source string) bool {
	return UITransportSource(source) || source == SourceCLI
}

func AttendedSource(source string) bool {
	return UITransportSource(source) || source == SourceCLI || source == SourceACP
}

func CreateSurface(source string) string {
	if source == SourceDesktop {
		return "gui"
	}
	return source
}

func SessionSource(platform string, e SourceEnv) string {
	source := strings.TrimFunc(e.Source, textSpace)
	if e.SingleQuery && !e.Explicit && UITransportSource(source) {
		source = ""
	}
	if e.SingleQuery && source == "" && (platform == "" || platform == SourceCLI) {
		return SourceOneshot
	}
	if source != "" {
		return source
	}
	if platform != "" {
		return platform
	}
	return SourceCLI
}

func Attended(platform string, e SourceEnv, finiteTurn bool) bool {
	if platform == "" || platform == PlatformSubagent {
		return false
	}
	if finiteTurn {
		return false
	}
	return AttendedSource(SessionSource(platform, e))
}
