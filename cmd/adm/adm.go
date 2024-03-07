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

package adm

import (
	"github.com/NVIDIA/kubectl-nv/cmd/adm/mustgather"
	"github.com/NVIDIA/kubectl-nv/internal/logger"

	cli "github.com/urfave/cli/v2"
)

type command struct {
	logger *logger.FunLogger
}

// NewCommand constructs a validate command with the specified logger
func NewCommand(logger *logger.FunLogger) *cli.Command {
	c := command{
		logger: logger,
	}
	return c.build()
}

func (m command) build() *cli.Command {
	// Create the 'adm' command
	adm := cli.Command{
		Name:  "adm",
		Usage: "Perform admin level operations on cluster NV resources",
	}

	adm.Subcommands = []*cli.Command{
		mustgather.NewCommand(m.logger),
	}

	return &adm
}
