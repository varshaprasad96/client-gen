/*
Copyright 2022 The KCP Authors.

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

package clientgen

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"k8s.io/code-generator/cmd/client-gen/args"
	"k8s.io/code-generator/cmd/client-gen/types"
	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
	"sigs.k8s.io/controller-tools/pkg/markers"

	"github.com/kcp-dev/code-generator/pkg/flag"
	"github.com/kcp-dev/code-generator/pkg/internal"
	"github.com/kcp-dev/code-generator/pkg/util"
)

var (
	// RuleDefinition is a marker for defining rules
	ruleDefinition = markers.Must(markers.MakeDefinition("genclient", markers.DescribesType, placeholder{}))
	// nonNamespacedMarker checks if resource is namespaced or clusterscoped
	nonNamespacedMarker = markers.Must(markers.MakeDefinition("genclient:nonNamespaced", markers.DescribesType, placeholder{}))
	// noStatusMarker checks if status is to scaffolded
	noStatusMarker = markers.Must(markers.MakeDefinition("+genclient:noStatus", markers.DescribesType, placeholder{}))
)

const (
	// GeneratorName is the name of the generator.
	GeneratorName = "client"
	// packageName for typed client wrappers.
	typedPackageName = "typed"
	// name of the file while wrapped clientset is written.
	clientSetFilename = "clientset.go"
	// extension for go file.
	extensionGo = ".go"
)

// Assigning marker's output to a placeholder struct, to verify to
// typecast the result and make sure if it exists for the type.
type placeholder struct{}

type Generator struct {
	// inputDir is the path where types are defined.
	inputDir string
	// inputpkgPaths stores details on input directory.
	inputpkgPaths pkgPaths
	// outputpkgPaths stores details on output directory.
	outputpkgPaths pkgPaths
	// output Dir where the wrappers are to be written.
	outputDir string
	// path to where generated clientsets are found.
	clientSetAPIPath string
	// clientsetName is the name of the generated clientset package.
	clientsetName string
	// GroupVersions for whom the clients are to be generated.
	groupVersions []types.GroupVersions
	// headerText is the header text to be added to generated wrappers.
	// It is obtained from `--go-header-text` flag.
	headerText string
}

// TODO: Store this information in generation context, as other genrators
// may need this too.
type pkgPaths struct {
	// basePacakge path as found in go module.
	basePackage string
	// hasGoMod is a way of checking if the go.mod file is present inside
	// the input directory or not. If present the basepkg path need not be modified
	// to include the location of input directory. If not, include the location of
	// all the sub folders provided in the input directory.
	hasGoMod bool
}

func (g Generator) RegisterMarker() (*markers.Registry, error) {
	reg := &markers.Registry{}
	if err := markers.RegisterAll(reg, ruleDefinition, nonNamespacedMarker, noStatusMarker); err != nil {
		return nil, fmt.Errorf("error registering markers")
	}
	return reg, nil
}

func (g Generator) GetName() string {
	return GeneratorName
}

// Run validates the input from the flags and sets default values, after which
// it calls the custom client genrator to create wrappers. If there are any
// errors while generating interface wrappers, it prints it out.
func (g Generator) Run(ctx *genall.GenerationContext, f flag.Flags) error {
	if err := validateFlags(f); err != nil {
		return err
	}

	if err := g.setDefaults(f); err != nil {
		return err
	}
	if err := g.generate(ctx); err != nil {
		return err
	}

	// print all the errors consolidated from packages in the generation context.
	// skip the type errors since they occur when input path does not contain
	// go.mod files.
	hadErr := loader.PrintErrors(ctx.Roots, packages.TypeError)
	if hadErr {
		return fmt.Errorf("generator did not run successfully")
	}
	return nil
}

// validateFlags checks if the inputs provided through flags are valid and
// if so, sets defaults.
func validateFlags(f flag.Flags) error {
	if f.InputDir == "" {
		return errors.New("input path to API definition is required.")
	}

	if f.ClientsetAPIPath == "" {
		return errors.New("specifying client API path is required currently.")
	}

	if len(f.GroupVersions) == 0 {
		return errors.New("list of group versions for which the clients are to be generated is required.")
	}

	return nil
}

// setDefaults sets the default values for the generator. It also creates
// a list of group versions provided as an input.
func (g *Generator) setDefaults(f flag.Flags) (err error) {
	if f.InputDir != "" {
		g.inputDir = f.InputDir
		pkg, hasGoMod := util.CurrentPackage(f.InputDir)
		if len(pkg) == 0 {
			return fmt.Errorf("error finding the module path for this package %q", f.InputDir)
		}
		g.inputpkgPaths = pkgPaths{
			basePackage: pkg,
			hasGoMod:    hasGoMod,
		}
	}
	if f.OutputDir != "" {
		pkg, hasGoMod := util.CurrentPackage(f.OutputDir)
		if len(pkg) == 0 {
			return fmt.Errorf("error finding the module path for this package %q", f.OutputDir)
		}
		g.outputpkgPaths = pkgPaths{
			basePackage: pkg,
			hasGoMod:    hasGoMod,
		}
		g.outputDir = f.OutputDir
	}
	if f.ClientsetAPIPath != "" {
		g.clientSetAPIPath = f.ClientsetAPIPath
	}
	if f.ClientsetName != "" {
		g.clientsetName = f.ClientsetName
	}
	g.headerText, err = getHeaderText(f.GoHeaderFilePath)
	if err != nil {
		return err
	}
	return g.getGV(f)
}

// getHeaderText reads the text passed through the file present in the
// path.
func getHeaderText(path string) (string, error) {
	var headertext string
	if path != "" {
		headerBytes, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		headertext = string(headerBytes)
	}
	return headertext, nil
}

// getGV parses the Group Versions provided in the input through flags
// and creates a list of []types.GroupVersions.
func (g *Generator) getGV(f flag.Flags) error {
	// Its already validated that list of group versions cannot be empty.
	gvs := f.GroupVersions
	for _, gv := range gvs {
		// arr[0] -> group, arr[1] -> versions
		arr := strings.Split(gv, ":")
		if len(arr) != 2 {
			return fmt.Errorf("input to --group-version must be in <group>:<versions> format, ex: rbac:v1. Got %q", gv)
		}

		versions := strings.Split(arr[1], ",")
		for _, v := range versions {
			// input path is converted to <inputDir>/<group>/<version>.
			// example for input directory of "k8s.io/client-go/kubernetes/pkg/apis/", it would
			// be converted to "k8s.io/client-go/kubernetes/pkg/apis/rbac/v1".
			input := filepath.Join(f.InputDir, arr[0], v)
			groups := []types.GroupVersions{}
			builder := args.NewGroupVersionsBuilder(&groups)
			_ = args.NewGVPackagesValue(builder, []string{input})

			g.groupVersions = append(g.groupVersions, groups...)

		}
	}
	return nil
}

// generate first generates the wrapper for all the interfaces provided in the input.
// Then for each type defined in the input, it recursively wraps the subsequent
// interfaces to be kcp-aware.
func (g *Generator) generate(ctx *genall.GenerationContext) error {
	if err := g.writeWrappedClientSet(); err != nil {
		return err
	}
	return g.generateSubInterfaces(ctx)
}

func (g *Generator) writeWrappedClientSet() error {
	var out bytes.Buffer
	if err := g.writeHeader(&out); err != nil {
		return err
	}

	// Get the location of the typed wrapped clientset for imports.
	// Cases handled here, for example the scenarios could be:
	// Case 1:
	// if basePkg := k8s.io/kcp-dev; outputPkg := k8s.io/kcp-dev/output/examples
	// then typedPkgPath is k8s.io/kcp-dev/output/examples/
	// Case 2:
	// if basePkg := k8s.io/kcp-dev; outputPkg := ./output/examples
	// then typedPkgPath is k8s.io/kcp-dev/output/examples/
	// Case 3:
	// if basePkg := k8s.io/kcp-dev; outputPkg := .
	// then typedPkgPath is k8s.io/kcp-dev
	var typedPkgPath string
	if !g.outputpkgPaths.hasGoMod {
		typedPkgPath = filepath.Join(util.GetCleanRealtivePath(g.outputpkgPaths.basePackage, filepath.Clean(g.outputDir)), g.clientsetName)
	} else {
		typedPkgPath = filepath.Join(g.outputpkgPaths.basePackage, g.clientsetName)
	}

	wrappedInf, err := internal.NewInterfaceWrapper(g.clientSetAPIPath, g.clientsetName, typedPkgPath, g.groupVersions, &out)
	if err != nil {
		return err
	}

	if err := wrappedInf.WriteContent(); err != nil {
		return err
	}
	outBytes := out.Bytes()
	formattedBytes, err := format.Source(outBytes)
	if err != nil {
		return err
	} else {
		outBytes = formattedBytes
	}

	return g.writeContent(outBytes, clientSetFilename, filepath.Join(g.outputDir, g.clientsetName))
}

// wrtieContents creates a new file under the path <outputDir>/generated with
// the specified filename and write contents to it.
func (g *Generator) writeContent(outBytes []byte, filename string, path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		err = os.MkdirAll(path, 0755)
		if err != nil {
			return err
		}
	}

	outputFile, err := os.Create(filepath.Join(path, filename))
	if err != nil {
		return err
	}
	defer outputFile.Close()

	n, err := outputFile.Write(outBytes)
	if err != nil {
		return err
	}
	if n < len(outBytes) {
		return err
	}
	return nil
}

func (g *Generator) writeHeader(out io.Writer) error {
	n, err := out.Write([]byte(g.headerText))
	if err != nil {
		return err
	}

	if n < len([]byte(g.headerText)) {
		return errors.New("header text was not written properly.")
	}
	return nil
}

func (g *Generator) generateSubInterfaces(ctx *genall.GenerationContext) error {
	for _, gv := range g.groupVersions {
		// Each types.GroupVersions will have only one version.
		// Even if there are multiple versions for same group, we will have separate types.GroupVersions
		// for it. Hence length of gv.Versions will always be one.
		version := gv.Versions[0]

		// This is to accomodate the usecase wherein the apis are defined under a sub-folder inside
		// base package.
		basePkg := g.inputpkgPaths.basePackage
		if !g.inputpkgPaths.hasGoMod {
			cleanPkgPath := util.CleanInputDir(g.inputDir)
			if cleanPkgPath != "" {
				basePkg = filepath.Join(g.inputpkgPaths.basePackage, cleanPkgPath)
			}
		}

		path := filepath.Join(basePkg, gv.Group.String(), string(version.Version))

		pkgs, err := loader.LoadRootsWithConfig(&packages.Config{Dir: g.inputDir}, path)
		if err != nil {
			return err
		}

		// Assign the pkgs obtained from loading roots to generation context.
		// TODO: Figure out if controller-tools generation runtime can be used to
		// wire in instead.
		ctx.Roots = pkgs

		for _, root := range pkgs {
			root.NeedTypesInfo()

			// this is to accomodate multiple types defined in single group
			byType := make(map[string][]byte)

			var outCommonContent bytes.Buffer
			pkgmg := internal.NewPackages(root, path, g.clientSetAPIPath, string(version.Version), gv.PackageName, &outCommonContent)

			if err := g.writeHeader(&outCommonContent); err != nil {
				root.AddError(err)
			}
			err = pkgmg.WriteContent()
			if err != nil {
				root.AddError(err)
			}

			if eachTypeErr := markers.EachType(ctx.Collector, root, func(info *markers.TypeInfo) {
				var outContent bytes.Buffer

				// if not enabled for this type, skip
				if !isEnabledForMethod(info) {
					return
				}

				a, err := internal.NewAPI(root, info, string(version.Version), gv.PackageName, !isClusterScoped(info), hasStatusSubresource(info), &outContent)
				if err != nil {
					root.AddError(err)
					return
				}

				err = a.WriteContent()
				if err != nil {
					root.AddError(err)
					return
				}

				outBytes := outContent.Bytes()
				if len(outBytes) > 0 {
					byType[info.Name] = outBytes
				}
			}); eachTypeErr != nil {
				return eachTypeErr
			}

			if len(byType) == 0 {
				return nil
			}

			var outContent bytes.Buffer
			outContent.Write(outCommonContent.Bytes())
			err = writeMethods(&outContent, byType)
			if err != nil {
				return err
			}

			outBytes := outContent.Bytes()
			formattedBytes, err := format.Source(outBytes)
			if err != nil {
				root.AddError(err)
			} else {
				outBytes = formattedBytes
			}

			filename := gv.Group.PackageName() + string(version.Version) + extensionGo
			err = g.writeContent(outBytes, filename, filepath.Join(g.outputDir, g.clientsetName, typedPackageName, gv.Group.PackageName(), string(version.Version)))
			if err != nil {
				root.AddError(err)
				return err
			}
		}
	}
	return nil
}

// isEnabledForMethod verifies if the genclient marker is enabled for
// this type or not.
func isEnabledForMethod(info *markers.TypeInfo) bool {
	enabled := info.Markers.Get(ruleDefinition.Name)
	return enabled != nil
}

// isClusterScoped verifies if the genclient marker for this
// type is namespaced or clusterscoped.
func isClusterScoped(info *markers.TypeInfo) bool {
	enabled := info.Markers.Get(nonNamespacedMarker.Name)
	return enabled != nil
}

// hasStatusSubresource verifies if updateStatus verb is to be scaffolded.
// if `noStatus` marker is present is returns false. Else it checks if
// the type has Status field.
func hasStatusSubresource(info *markers.TypeInfo) bool {
	if info.Markers.Get(noStatusMarker.Name) != nil {
		return false
	}

	hasStatusField := false
	for _, f := range info.Fields {
		if f.Name == "Status" {
			hasStatusField = true
			break
		}
	}
	return hasStatusField
}

func writeMethods(out io.Writer, byType map[string][]byte) error {
	sortedNames := make([]string, 0, len(byType))
	for name := range byType {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)

	for _, name := range sortedNames {
		_, err := out.Write(byType[name])
		if err != nil {
			return err
		}
	}
	return nil
}
