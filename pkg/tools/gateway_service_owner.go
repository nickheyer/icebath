package tools

import "regexp"

var (
	serviceEnvHomeRe      = regexp.MustCompile(`(?:ICEBATH|HERMES)_HOME=([^"\r\n]+)`)
	servicePlistHomeRe    = regexp.MustCompile(`(?s)<key>(?:ICEBATH|HERMES)_HOME</key>\s*<string>(.*?)</string>`)
	serviceVBSHomeRe      = regexp.MustCompile(`Item\("(?:ICEBATH|HERMES)_HOME"\)\s*=\s*"([^"\r\n]+)"`)
	serviceLaunchScriptRe = regexp.MustCompile(`(?i)"((?:[A-Za-z]:[\\/]|\\\\|/)[^"\r\n]*\.(?:vbs|cmd))"`)
)

// Every home a gateway service definition sets through systemd, launchd, gateway.cmd and gateway.vbs syntax
func serviceDefinitionHomes(definition string) []string {
	var homes []string
	for _, re := range []*regexp.Regexp{serviceEnvHomeRe, servicePlistHomeRe, serviceVBSHomeRe} {
		for _, m := range re.FindAllStringSubmatch(definition, -1) {
			homes = append(homes, m[1])
		}
	}
	return homes
}

// Every home a gateway service definition sets, for the service writers outside this package
func ServiceDefinitionHomes(definition string) []string { return serviceDefinitionHomes(definition) }

// Scripts a definition starts by their quoted absolute paths, each once in first seen order
func serviceDefinitionLaunchers(definition string) []string {
	var found []string
	seen := map[string]bool{}
	for _, m := range serviceLaunchScriptRe.FindAllStringSubmatch(definition, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			found = append(found, m[1])
		}
	}
	return found
}
