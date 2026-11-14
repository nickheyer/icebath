package main

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/trajectorycompressor"
)

// Runs the trajectory compressor command line and exits with its status
func main() {
	os.Exit(trajectorycompressor.Main(context.Background(), os.Args[1:]))
}
