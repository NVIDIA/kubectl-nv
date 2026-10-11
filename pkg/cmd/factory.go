/*
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package cmd holds what every kubectl-nv command needs before it can talk to a
// cluster: the kubectl configuration flags, the resolved kubeconfig path, and
// the factory the commands get their clients from.
package cmd

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/scheme"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// Factory hands a command the clients it needs. There is one implementation
// against a real cluster, built from the kubectl configuration flags; tests
// supply their own so that no test reaches a cluster.
type Factory interface {
	KubernetesClient() (kubernetes.Interface, error)
	DynamicClient() (dynamic.Interface, error)
	RESTConfig() (*rest.Config, error)
}

// Options is the state the root command resolves once and every subcommand
// reads.
type Options struct {
	ConfigFlags *genericclioptions.ConfigFlags
	Streams     genericiooptions.IOStreams
	Factory     Factory

	// Debug reports whether debug-level logging was asked for, by --debug, -d or
	// the DEBUG environment variable.
	Debug bool

	// Logger is where commands report progress. It writes to the error stream
	// only, so stdout stays the command's own output.
	Logger *output.Logger
}

// Log returns where this command reports progress, making a plain one over the
// error stream when the root has not set one, so that a command built by hand
// in a test still has somewhere to write.
func (o *Options) Log() *output.Logger {
	if o.Logger == nil {
		o.Logger = output.NewLogger(o.Streams, output.LoggerOptions{})
	}
	return o.Logger
}

// KubeconfigDisplay names the kubeconfig the clients will be built from, for
// reporting it to the user. It asks the client configuration rather than
// resolving a path of its own: a second resolver can disagree with the one
// that actually loads the file, and then the gather names a cluster it never
// looked at.
func (o *Options) KubeconfigDisplay() string {
	if o.ConfigFlags == nil {
		return ""
	}
	// The loading precedence is a single entry when --kubeconfig named one, and
	// the KUBECONFIG list or the default path otherwise, which is exactly what
	// there is to report.
	access := o.ConfigFlags.ToRawKubeConfigLoader().ConfigAccess()
	return strings.Join(access.GetLoadingPrecedence(), string(os.PathListSeparator))
}

// AddConfigFlags registers the kubectl configuration flags on flags, giving
// --kubeconfig the -k shorthand that kubectl-nv accepted before it moved to
// cobra.
func AddConfigFlags(flags *pflag.FlagSet, configFlags *genericclioptions.ConfigFlags) {
	scratch := pflag.NewFlagSet("kubectl-nv-config", pflag.ContinueOnError)
	configFlags.AddFlags(scratch)
	scratch.VisitAll(func(f *pflag.Flag) {
		if f.Name == "kubeconfig" {
			f.Shorthand = "k"
		}
		flags.AddFlag(f)
	})
}

// UsageArgs wraps a positional argument validator so that a failure carries
// its own exit code.
//
// Without it the code depends on who runs the command: cmd/kubectl-nv turns an
// unsilenced error into a usage one, but a command executed any other way, by
// a test or by a future embedder, gets a bare error and so exit 4. Exit 4
// means the run could not happen, which tells a script that the cluster is
// unreachable when the truth is that the user typed too many words.
func UsageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if err := validate(c, args); err != nil {
			return output.WrapUsageError(err, "%s", c.CommandPath())
		}

		return nil
	}
}

// NewFactory returns the Factory that talks to the cluster the configuration
// flags point at. The REST configuration and the clients are built once and
// reused, so a command that needs two of them does not read the kubeconfig
// twice.
func NewFactory(configFlags *genericclioptions.ConfigFlags) Factory {
	return &configFlagsFactory{configFlags: configFlags}
}

type configFlagsFactory struct {
	configFlags *genericclioptions.ConfigFlags

	mu        sync.Mutex
	config    *rest.Config
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

func (f *configFlagsFactory) RESTConfig() (*rest.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restConfig()
}

// restConfig builds the REST configuration. The caller holds f.mu.
func (f *configFlagsFactory) restConfig() (*rest.Config, error) {
	if f.config != nil {
		return f.config, nil
	}
	config, err := f.configFlags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("building the kubernetes client configuration: %w", err)
	}
	// The copy-from-pod path serialises its own requests against the core API, so
	// the group version and codecs have to be set on the shared configuration.
	if err := setKubernetesDefaults(config); err != nil {
		return nil, fmt.Errorf("applying kubernetes client defaults: %w", err)
	}
	f.config = config
	return config, nil
}

func (f *configFlagsFactory) KubernetesClient() (kubernetes.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clientset != nil {
		return f.clientset, nil
	}
	config, err := f.restConfig()
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating the kubernetes client: %w", err)
	}
	f.clientset = clientset
	return clientset, nil
}

func (f *configFlagsFactory) DynamicClient() (dynamic.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dynamic != nil {
		return f.dynamic, nil
	}
	config, err := f.restConfig()
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating the dynamic client: %w", err)
	}
	f.dynamic = client
	return client, nil
}

// setKubernetesDefaults sets default values on the provided client config for
// accessing the Kubernetes API or returns an error if any of the defaults are
// impossible or invalid.
func setKubernetesDefaults(config *rest.Config) error {
	config.GroupVersion = &schema.GroupVersion{Group: "", Version: "v1"}

	if config.APIPath == "" {
		config.APIPath = "/api"
	}
	if config.NegotiatedSerializer == nil {
		// This codec factory ensures the resources are not converted. Therefore,
		// resources will not be round-tripped through internal versions. Defaulting
		// does not happen on the client.
		config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	}
	return rest.SetKubernetesDefaults(config)
}
