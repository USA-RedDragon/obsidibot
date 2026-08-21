package main

import (
	"context"
	"os"

	"github.com/USA-RedDragon/configulator"
	"github.com/USA-RedDragon/obsidibot/internal/cmd"
	"github.com/USA-RedDragon/obsidibot/internal/config"
)

// https://goreleaser.com/cookbooks/using-main.version/
//
//nolint:gochecknoglobals
var (
	version = "dev"
	commit  = "none"
)

func main() {
	rootCmd := cmd.New(version, commit, migrations())

	c := configulator.New[config.Config]().
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{
			Separator: "_",
		}).
		WithFile(&configulator.FileOptions{
			Paths: []string{"config.yaml"},
		}).
		WithPFlags(rootCmd.Flags(), nil)

	rootCmd.SetContext(c.WithContext(context.TODO()))

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
