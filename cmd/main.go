/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"os"

	"github.com/NVIDIA/kubectl-nv/cmd/adm"
	"github.com/NVIDIA/kubectl-nv/internal/logger"

	cli "github.com/urfave/cli/v2"
)

var log = logger.NewLogger()

type config struct {
	Debug bool
}

func main() {
	config := config{}

	// Create the top-level CLI
	c := cli.NewApp()
	c.Name = "kubectl-nv"
	c.Usage = "A kubectl plugin for NVIDIA powered Kubernetes clusters"
	c.Version = "0.1.0"

	// Setup the flags for this command
	c.Flags = []cli.Flag{
		&cli.BoolFlag{
			Name:        "debug",
			Aliases:     []string{"d"},
			Usage:       "Enable debug-level logging",
			Destination: &config.Debug,
			EnvVars:     []string{"DEBUG"},
		},
	}

	// Define the subcommands
	c.Commands = []*cli.Command{
		adm.NewCommand(log),
	}

	err := c.Run(os.Args)
	if err != nil {
		log.Error(err)
		log.Exit(1)
	}
}
