package matrix

// Go command that builds the binary with the pure Go olm implementation end to end encryption needs
const E2EEBuildCommand = "go build -tags goolm ./cmd/icebath"

// Go fact named where hermes names its missing E2EE packages and their pip install command
const E2EEMissingFact = "This binary was built without Matrix encryption. Build with: " + E2EEBuildCommand
