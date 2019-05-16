package commands

import (
	"fmt"
	"github.com/distributed-containers-inc/sanic/config"
	"github.com/distributed-containers-inc/sanic/shell"
	"github.com/urfave/cli"
	"strings"
)

func environmentCommandAction(c *cli.Context) error {
	if c.NArg() != 1 {
		return newUsageError(c)
	}

	sanicEnv := c.Args().First()
	sanicConfig, err := findSanicConfig()
	if err != nil {
		return wrapErrorWithExitCode(err, 1)
	}
	if sanicConfig == "" {
		return cli.NewExitError(fmt.Sprintf("this command requires a %s file in your current dirctory or a parent directory.", SanicConfigName), 1)
	}

	return wrapErrorWithExitCode(
		shell.Enter(sanicEnv, sanicConfig),
		1)
}

func environmentCommandAutocomplete(c *cli.Context) {
	if c.NArg() > 1 {
		return
	}
	var requestedEnv = ""
	if c.NArg() == 1 {
		requestedEnv = c.Args().First()
	}
	configPath, err := findSanicConfig()
	if err != nil {
		return
	}
	configData, err := config.ReadFromPath(configPath)
	if err != nil || configData == nil {
		return
	}

	var possibleEnvs = []string{}

	for key := range configData.Environments {
		if (strings.HasPrefix(key, requestedEnv)) {
			possibleEnvs = append(possibleEnvs, key)
		}
	}
	if len(possibleEnvs) == 1 {
		print(possibleEnvs[0])
	}
	for env := range possibleEnvs {
		println(env)
	}
}

var EnvironmentCommand = cli.Command{
	Name:         "env",
	Usage:        "change to a specific (e.g., dev or production) environment named in the configuration",
	ArgsUsage:    "[environment name]",
	Action:       environmentCommandAction,
	BashComplete: environmentCommandAutocomplete,
}