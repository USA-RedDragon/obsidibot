package main

import (
	"context"
	"os"

	configulator "github.com/USA-RedDragon/configulator/v2"
	cpflag "github.com/USA-RedDragon/configulator/v2/flags/pflag"
	"github.com/USA-RedDragon/obsidibot/internal/cmd"
	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/goccy/go-yaml"
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

	c := configulator.New(config.ConfigSchema()).
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{
			Separator: "_",
		}).
		WithFile(&configulator.FileOptions{
			Search:   []string{"config.yaml"},
			Decoders: configulator.Decoders{".yaml": yaml.Unmarshal, ".yml": yaml.Unmarshal},
		})
	cpflag.Bind(c, rootCmd.Flags(), config.ConfigPFlagHooks(), nil)

	rootCmd.SetContext(c.WithContext(context.TODO()))

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
