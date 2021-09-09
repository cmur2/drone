// Copyright 2019 Drone IO, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package starlark

import (
	"bytes"
	"io/ioutil"
	"path"

	"github.com/drone/drone/core"
	"github.com/drone/drone/handler/api/errors"

	"github.com/bazelbuild/bazel-gazelle/label"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/sirupsen/logrus"
	"go.starlark.net/starlark"
)

const (
	separator = "---"
	newline   = "\n"
)

// limits generated configuration file size.
const limit = 1000000

var (
	// ErrMainMissing indicates the starlark script is missing
	// the main method.
	ErrMainMissing = errors.New("starlark: missing main function")

	// ErrMainInvalid indicates the starlark script defines a
	// global variable named main, however, it is not callable.
	ErrMainInvalid = errors.New("starlark: main must be a function")

	// ErrMainReturn indicates the starlark script's main method
	// returns an invalid or unexpected type.
	ErrMainReturn = errors.New("starlark: main returns an invalid type")

	// ErrMaximumSize indicates the starlark script generated a
	// file that exceeds the maximum allowed file size.
	ErrMaximumSize = errors.New("starlark: maximum file size exceeded")

	// ErrCannotLoad indicates the starlark script is attempting to
	// load an external file which is currently restricted.
	ErrCannotLoad = errors.New("starlark: cannot load external scripts")
)

func Parse(req *core.ConvertArgs, template *core.Template, templateData map[string]interface{}, stepLimit uint64) (string, error) {
	thread := &starlark.Thread{
		Name: "drone",
		Load: loadExtension,
		Print: func(_ *starlark.Thread, msg string) {
			logrus.WithFields(logrus.Fields{
				"namespace": req.Repo.Namespace,
				"name":      req.Repo.Name,
			}).Traceln(msg)
		},
	}
	var starlarkFile string
	var starlarkFileName string
	if template != nil {
		starlarkFile = template.Data
		starlarkFileName = template.Name
	} else {
		starlarkFile = req.Config.Data
		starlarkFileName = req.Repo.Config
	}

	globals, err := starlark.ExecFile(thread, starlarkFileName, starlarkFile, nil)
	if err != nil {
		return "", err
	}

	// find the main method in the starlark script and
	// cast to a callable type. If not callable the script
	// is invalid.
	mainVal, ok := globals["main"]
	if !ok {
		return "", ErrMainMissing
	}
	main, ok := mainVal.(starlark.Callable)
	if !ok {
		return "", ErrMainInvalid
	}

	// create the input args and invoke the main method
	// using the input args.
	args, err := createArgs(req.Repo, req.Build, templateData)
	if err != nil {
		return "", err
	}

	// set the maximum number of operations in the script. this
	// mitigates long running scripts.
	if stepLimit == 0 {
		stepLimit = 50000
	}
	thread.SetMaxExecutionSteps(stepLimit)

	// execute the main method in the script.
	mainVal, err = starlark.Call(thread, main, args, nil)
	if err != nil {
		return "", err
	}

	buf := new(bytes.Buffer)
	switch v := mainVal.(type) {
	case *starlark.List:
		for i := 0; i < v.Len(); i++ {
			item := v.Index(i)
			buf.WriteString(separator)
			buf.WriteString(newline)
			if err := write(buf, item); err != nil {
				return "", err
			}
			buf.WriteString(newline)
		}
	case *starlark.Dict:
		if err := write(buf, v); err != nil {
			return "", err
		}
	default:
		return "", ErrMainReturn
	}

	// this is a temporary workaround until we
	// implement a LimitWriter.
	if b := buf.Bytes(); len(b) > limit {
		return "", ErrMaximumSize
	}
	return buf.String(), nil
}

// TODO: https://github.com/drone/drone/blob/ca454594021099909fb4ee9471720cacfe3207bd/scripts/build.sh#L8

// Source: https://github.com/drone/drone-convert-starlark/commit/0a532618c6ee7705964762efe37e380865643a8a
func loadExtension(thread *starlark.Thread, labelStr string) (starlark.StringDict, error) {
	// Breaks a label string into a struct that separates out the
	// repo name, package path, and extension name.
	parsedLabel, err1 := label.Parse(labelStr)
	if err1 != nil {
		return nil, err1
	}

	// We don't (yet) support loading extensions from within the repo
	// that is being built.
	if parsedLabel.Repo == "" {
		return nil, errors.New("loadExtension: label's repo cannot be empty")
	}

	if parsedLabel.Relative {
		return nil, errors.New("loadExtension: label cannot be relative")
	}

	// https://docs.bazel.build/versions/main/build-ref.html#load

	// extensionPath := path.Join("/var/starlark-repo/", parsedLabel.Repo, parsedLabel.Pkg, parsedLabel.Name)
	extensionPath, err2 := securejoin.SecureJoin("/var/starlark-repo", path.Join(parsedLabel.Repo, parsedLabel.Pkg, parsedLabel.Name))
	if err2 != nil {
		return nil, err2
	}

	extensionContents, err3 := ioutil.ReadFile(extensionPath)
	if err3 != nil {
		return nil, err3
	}

	globals, err4 := starlark.ExecFile(thread, extensionPath, extensionContents, nil)
	if err4 != nil {
		return nil, err4
	}

	return globals, nil
}
