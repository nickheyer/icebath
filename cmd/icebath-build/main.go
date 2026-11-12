package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/selfupdate"
)

// Builds the dashboard bundle a release binary embeds into cmd/icebath/web_dist of the checkout
func main() {
	if len(os.Args) < 2 || os.Args[1] != "web" {
		fmt.Fprintln(os.Stderr, "usage: icebath-build web [--source DIR] [--icons DIR]")
		os.Exit(2)
	}
	flags := flag.NewFlagSet("web", flag.ExitOnError)
	source := flags.String("source", ".", "checkout to build from")
	icons := flags.String("icons", "", "prepared icons tree, the checkout's committed icons when empty")
	flags.Parse(os.Args[2:])
	root, err := filepath.Abs(*source)
	if err == nil {
		err = buildWeb(context.Background(), root, *icons)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "icebath-build:", err)
		os.Exit(1)
	}
}

// Prepares the web workspace dependencies under the explicit build environment and compiles the bundle
func buildWeb(ctx context.Context, root, icons string) error {
	env, err := selfupdate.SourceBuildEnv(ctx, nil, true, true)
	if err != nil {
		return err
	}
	if err := selfupdate.PrepareSourceDependencies(ctx, root, []string{"web"}, env, true); err != nil {
		return err
	}
	return selfupdate.BuildSourceWeb(ctx, root, env, icons)
}
