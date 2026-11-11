package gateway

import "strings"

// Argv word that runs the gateway module entry in a child of this binary, the python -m gateway.run of hermes
const GatewayRunModuleEntry = "__icebath_gateway_run__"

// Whether lowered command line tokens run the gateway module entry of an icebath binary
func runsGatewayModuleEntry(tokens []string) bool {
	for i := 0; i+1 < len(tokens); i++ {
		base := tokens[i][strings.LastIndex(tokens[i], "/")+1:]
		if (base == "icebath" || base == "icebath.exe") && tokens[i+1] == GatewayRunModuleEntry {
			return true
		}
	}
	return false
}
