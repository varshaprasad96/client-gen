/*
Copyright The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import (
	"log"
	"os"

	"github.com/kcp-dev/client-gen/pkg/flag"
	"github.com/kcp-dev/client-gen/pkg/generator"
	"github.com/spf13/pflag"
	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/markers"
)

func main() {

	// TODO: convert this to a cobra command if required
	f := flag.Flags{}
	f.AddTo(pflag.CommandLine)
	pflag.Parse()

	// Register genclient marker to rule definition
	reg := &markers.Registry{}
	if err := reg.Register(generator.RuleDefinition); err != nil {
		log.Fatalf(err.Error())
		os.Exit(1)
	}

	ctx := &genall.GenerationContext{Collector: &markers.Collector{Registry: reg}}
	g := generator.Generator{}
	if err := g.Run(ctx, f); err != nil {
		log.Fatalf(err.Error())
		os.Exit(1)
	}
}