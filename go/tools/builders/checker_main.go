/* Copyright 2018 The Bazel Authors. All rights reserved.

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

// Loads and runs registered analyses on a well-typed Go package.
// The code in this file is combined with the code generated by
// generate_checker_main.go.

package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bazelbuild/rules_go/go/tools/analysis"
	"golang.org/x/tools/go/gcexportdata"
)

// run returns an error only if the package is successfully loaded and at least
// one analysis fails. All other errors (e.g. during loading) are logged but
// do not return an error so as not to unnecessarily interrupt builds.
func run(args []string) error {
	archiveFiles := multiFlag{}
	flags := flag.NewFlagSet("checker", flag.ContinueOnError)
	flags.Var(&archiveFiles, "archivefile", "Archive file of a direct dependency")
	stdlib := flags.String("stdlib", "", "Root directory of stdlib")
	if err := flags.Parse(args); err != nil {
		log.Println(err)
		return nil
	}
	if *stdlib == "" {
		log.Printf("missing stdlib root directory")
		return nil
	}
	importsToArchives := make(map[string]string)
	for _, a := range archiveFiles {
		kv := strings.Split(a, "=")
		if len(kv) != 2 {
			continue // sanity check
		}
		importsToArchives[kv[0]] = kv[1]
	}
	fset := token.NewFileSet()
	imp := &importer{
		fset:              fset,
		packages:          make(map[string]*types.Package),
		importsToArchives: importsToArchives,
		stdlib:            *stdlib,
	}
	apkg, err := load(fset, imp, flags.Args())
	if err != nil {
		log.Printf("error loading package: %v\n", err)
		return nil
	}

	c := make(chan result)
	// Perform analyses in parallel.
	for _, a := range analysis.Analyses() {
		go func(a *analysis.Analysis) {
			defer func() {
				// Prevent a panic in a single analysis from interrupting other analyses.
				if r := recover(); r != nil {
					c <- result{name: a.Name, err: fmt.Errorf("panic : %v", r)}
				}
			}()
			res, err := a.Run(apkg)
			switch err {
			case nil:
				c <- result{name: a.Name, findings: res.Findings}
			case analysis.ErrSkipped:
				c <- result{name: a.Name, err: fmt.Errorf("skipped : %v", err)}
			default:
				c <- result{name: a.Name, err: fmt.Errorf("internal error: %v", err)}
			}
		}(a)
	}
	// Collate analysis results.
	var allFindings []*analysis.Finding
	failBuild := false
	for i := 0; i < len(analysis.Analyses()); i++ {
		result := <-c
		if result.err != nil {
			// Analysis failed or skipped.
			log.Printf("analysis %q %v", result.name, result.err)
			continue
		}
		if len(result.findings) == 0 {
			continue
		}
		config, ok := configs[result.name]
		if !ok {
			// The default behavior is not to fail builds but print analysis findings.
			allFindings = append(allFindings, result.findings...)
			continue
		}
		if config.severity == severityError {
			failBuild = true
		}
		// Discard findings based on the check configuration.
		for _, finding := range result.findings {
			filename := fset.File(finding.Pos).Name()
			include := true
			if len(config.applyTo) > 0 {
				// This analysis applies exclusively to a set of files.
				include = false
				for pattern := range config.applyTo {
					if matched, err := regexp.MatchString(pattern, filename); err == nil && matched {
						include = true
					}
				}
			}
			for pattern := range config.whitelist {
				if matched, err := regexp.MatchString(pattern, filename); err == nil && matched {
					include = false
				}
			}
			if include {
				allFindings = append(allFindings, finding)
			}
		}
	}
	// Print analysis results, returning an error to fail the build if necessary.
	if len(allFindings) != 0 {
		sort.Slice(allFindings, func(i, j int) bool {
			return allFindings[i].Pos < allFindings[j].Pos
		})
		errMsg := "errors found during build-time code analysis:\n"
		for _, f := range allFindings {
			errMsg += fmt.Sprintf("%s: %s\n", fset.Position(f.Pos), f.Message)
		}
		if failBuild {
			return errors.New(errMsg)
		}
		log.Println(errMsg)
	}
	return nil
}

type config struct {
	severity           severity
	applyTo, whitelist map[string]bool
}

type severity uint8

const (
	severityWarning severity = iota
	severityError
)

func main() {
	log.SetFlags(0) // no timestamp
	log.SetPrefix("GoChecker: ")
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

type result struct {
	name      string
	findings  []*analysis.Finding
	err       error
	failBuild bool
}

// load parses and type checks the source code in each file in filenames.
func load(fset *token.FileSet, imp types.Importer, filenames []string) (*analysis.Package, error) {
	if len(filenames) == 0 {
		return nil, errors.New("no filenames")
	}
	var files []*ast.File
	for _, file := range filenames {
		f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}

	config := types.Config{
		Importer: imp,
		Error: func(err error) {
			e := err.(types.Error)
			msg := fmt.Sprintf("%s", e.Msg)
			posn := e.Fset.Position(e.Pos)
			if posn.Filename != "" {
				msg = fmt.Sprintf("%s: %s", posn, msg)
			}
			fmt.Fprintln(os.Stderr, msg)
		},
	}
	info := &types.Info{
		Types:     make(map[ast.Expr]types.TypeAndValue),
		Uses:      make(map[*ast.Ident]types.Object),
		Defs:      make(map[*ast.Ident]types.Object),
		Implicits: make(map[ast.Node]types.Object),
	}
	pkg, err := config.Check(files[0].Name.Name, fset, files, info)
	if err != nil {
		// Errors were already reported through config.Error.
		return nil, nil
	}
	return &analysis.Package{Fset: fset, Files: files, Types: pkg, Info: info}, nil
}

type importer struct {
	fset     *token.FileSet
	packages map[string]*types.Package
	// importsToArchives maps import paths to the path to the archive file representing the
	// corresponding library.
	importsToArchives map[string]string
	// stdlib is the root directory containing standard library package archive files.
	stdlib string
}

func (i *importer) Import(path string) (*types.Package, error) {
	archive, ok := i.importsToArchives[path]
	if !ok {
		// stdlib package.
		ctxt := build.Default
		archive = filepath.Join(i.stdlib, "pkg", ctxt.GOOS+"_"+ctxt.GOARCH, path+".a")
	}
	// open file
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer func() {
		f.Close()
		if err != nil {
			// add file name to error
			err = fmt.Errorf("reading export data: %s: %v", archive, err)
		}
	}()

	r, err := gcexportdata.NewReader(f)
	if err != nil {
		return nil, err
	}

	return gcexportdata.Read(r, i.fset, i.packages, path)
}
