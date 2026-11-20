package matrix

// Go command that builds the binary with the pure Go olm implementation end to end encryption needs
const E2EEBuildCommand = "go build -tags goolm ./cmd/icebath"

// Fact a build without Matrix encryption reports, naming the build command
const E2EEMissingFact = "This binary was built without Matrix encryption. Build with: " + E2EEBuildCommand
