package tools

import "regexp"

var (
	serviceEnvHomeRe   = regexp.MustCompile(`(?:ICEBATH|HERMES)_HOME=([^"\r\n]+)`)
	servicePlistHomeRe = regexp.MustCompile(`(?s)<key>(?:ICEBATH|HERMES)_HOME</key>\s*<string>(.*?)</string>`)
	serviceVBSHomeRe   = regexp.MustCompile(`Item\("(?:ICEBATH|HERMES)_HOME"\)\s*=\s*"([^"\r\n]+)"`)
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
