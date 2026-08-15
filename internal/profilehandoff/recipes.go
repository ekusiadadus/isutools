// Package profilehandoff generates deterministic, non-executing recipes for
// standard Go profiling tools.
package profilehandoff

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	SchemaV1            = "isutools.profile-handoff/v1"
	ModeInterval        = "interval"
	ModeCumulativeDelta = "cumulative-delta"
)

type InputFile struct {
	Point  string `json:"point"`
	File   string `json:"file"`
	SHA256 string `json:"sha256,omitempty"`
}

type ProfileInput struct {
	Kind            string       `json:"kind"`
	Mode            string       `json:"mode"`
	Binary          string       `json:"binary"`
	BinaryMatch     bool         `json:"binary_match"`
	SourceAvailable bool         `json:"source_available"`
	SourceRoot      string       `json:"source_root,omitempty"`
	HasLabels       bool         `json:"has_labels"`
	SampleTypes     []SampleType `json:"sample_types,omitempty"`
	Inputs          []InputFile  `json:"inputs"`
}

type SampleType struct {
	Type string `json:"type"`
	Unit string `json:"unit"`
}

type Recipe struct {
	Purpose    string   `json:"purpose"`
	Tool       string   `json:"tool"`
	Argv       []string `json:"argv"`
	Ready      bool     `json:"ready"`
	Code       string   `json:"code,omitempty"`
	Conditions []string `json:"conditions,omitempty"`
	SampleType string   `json:"sample_type,omitempty"`
	Unit       string   `json:"unit,omitempty"`
}

func Generate(input ProfileInput) ([]Recipe, error) {
	open, close, interval, err := validateProfileInput(input)
	if err != nil {
		return nil, err
	}
	makeArgs := func(options ...string) []string {
		args := []string{"go", "tool", "pprof"}
		if input.Mode == ModeCumulativeDelta {
			args = append(args, "-base", open)
		}
		args = append(args, options...)
		args = append(args, input.Binary)
		if input.Mode == ModeCumulativeDelta {
			args = append(args, close)
		} else {
			args = append(args, interval)
		}
		return args
	}
	specs := []struct {
		purpose string
		options []string
		source  bool
	}{
		{"pprof-web", []string{"-http=:0"}, false},
		{"pprof-top", []string{"-top"}, false},
		{"pprof-source", sourceOptions(input.SourceRoot, "-list=FUNCTION_REGEXP"), true},
		{"pprof-weblist", sourceOptions(input.SourceRoot, "-weblist=FUNCTION_REGEXP"), true},
		{"pprof-disasm", []string{"-disasm=FUNCTION_REGEXP"}, false},
		{"pprof-callers-callees", []string{"-peek=FUNCTION_REGEXP"}, false},
		{"pprof-focus", []string{"-focus=FUNCTION_REGEXP", "-http=:0"}, false},
		{"pprof-ignore", []string{"-ignore=FUNCTION_REGEXP", "-http=:0"}, false},
	}
	result := make([]Recipe, 0, len(specs)+1)
	for _, spec := range specs {
		recipe := Recipe{Purpose: spec.purpose, Tool: "go tool pprof", Argv: makeArgs(spec.options...), Ready: true}
		switch {
		case !input.BinaryMatch:
			recipe.Ready, recipe.Code = false, "binary-match-required"
			recipe.Conditions = []string{"provide a binary whose SHA-256 matches the capture manifest"}
		case spec.source && !input.SourceAvailable:
			recipe.Ready, recipe.Code = false, "source-unavailable"
			recipe.Conditions = []string{"provide the matching source revision or source search path"}
		}
		result = append(result, recipe)
	}
	if input.HasLabels {
		for _, spec := range []struct {
			purpose string
			option  string
		}{{"pprof-tagfocus", "-tagfocus=isutools_tuple=TUPLE_ID"}, {"pprof-tagignore", "-tagignore=isutools_tuple=TUPLE_ID"}} {
			recipe := Recipe{Purpose: spec.purpose, Tool: "go tool pprof", Argv: makeArgs(spec.option, "-http=:0"), Ready: input.BinaryMatch}
			if !recipe.Ready {
				recipe.Code = "binary-match-required"
			}
			result = append(result, recipe)
		}
	}
	if len(input.SampleTypes) > 1 {
		for _, sample := range input.SampleTypes {
			if !safeToken(sample.Type) || !safeToken(sample.Unit) {
				return nil, errors.New("profilehandoff: invalid sample type")
			}
			recipe := Recipe{Purpose: "pprof-sample-" + sample.Type, Tool: "go tool pprof", Argv: makeArgs("-sample_index=" + sample.Type), Ready: input.BinaryMatch, SampleType: sample.Type, Unit: sample.Unit}
			if !recipe.Ready {
				recipe.Code = "binary-match-required"
			}
			result = append(result, recipe)
		}
	}
	return result, nil
}

func sourceOptions(root, format string) []string {
	if root == "" {
		return []string{format}
	}
	return []string{"-source_path=" + root, format}
}

func GenerateTrace(file string, ready bool) ([]Recipe, error) {
	if !safeBasename(file, ".out") {
		return nil, errors.New("profilehandoff: unsafe trace file")
	}
	recipe := Recipe{Purpose: "trace-web", Tool: "go tool trace", Argv: []string{"go", "tool", "trace", file}, Ready: ready}
	if !ready {
		recipe.Code = "trace-incomplete"
		recipe.Conditions = []string{"use a complete, hash-verified trace artifact"}
	}
	return []Recipe{recipe}, nil
}

// Comparison generates an independent-run comparison. It deliberately uses
// -diff_base; cumulative open/close subtraction uses -base in Generate.
func Comparison(current, base ProfileInput, normalize bool) (Recipe, error) {
	_, _, currentFile, err := validateProfileInput(current)
	if err != nil {
		return Recipe{}, err
	}
	_, _, baseFile, err := validateProfileInput(base)
	if err != nil {
		return Recipe{}, err
	}
	if current.Mode != ModeInterval || base.Mode != ModeInterval || current.Kind != base.Kind || !current.BinaryMatch || !base.BinaryMatch || current.Binary != base.Binary {
		return Recipe{}, errors.New("profilehandoff: incompatible comparison inputs")
	}
	args := []string{"go", "tool", "pprof", "-diff_base", baseFile}
	if normalize {
		args = append(args, "-normalize")
	}
	args = append(args, "-http=:0", current.Binary, currentFile)
	return Recipe{Purpose: "pprof-independent-diff", Tool: "go tool pprof", Argv: args, Ready: true}, nil
}

func validateProfileInput(input ProfileInput) (open, close, interval string, err error) {
	if !safeToken(input.Kind) || input.Binary == "" || strings.ContainsAny(input.Binary, "\r\n\x00") ||
		strings.ContainsAny(input.SourceRoot, "\r\n\x00") || len(input.SourceRoot) > 4096 || (input.SourceAvailable && input.SourceRoot == "") {
		return "", "", "", errors.New("profilehandoff: invalid profile identity")
	}
	for _, file := range input.Inputs {
		if !safeBasename(file.File, ".pprof") {
			return "", "", "", errors.New("profilehandoff: unsafe profile file")
		}
		switch file.Point {
		case "open":
			open = file.File
		case "close":
			close = file.File
		case "interval":
			interval = file.File
		default:
			return "", "", "", errors.New("profilehandoff: invalid profile point")
		}
	}
	switch input.Mode {
	case ModeInterval:
		if len(input.Inputs) != 1 || interval == "" {
			return "", "", "", errors.New("profilehandoff: interval requires one input")
		}
	case ModeCumulativeDelta:
		if len(input.Inputs) != 2 || open == "" || close == "" || open == close {
			return "", "", "", errors.New("profilehandoff: cumulative delta requires open and close")
		}
	default:
		return "", "", "", errors.New("profilehandoff: unsupported mode")
	}
	return open, close, interval, nil
}

func RenderShell(argv []string) (string, error) {
	if len(argv) == 0 || len(argv) > 32 {
		return "", errors.New("profilehandoff: invalid argv")
	}
	quoted := make([]string, 0, len(argv))
	for _, argument := range argv {
		if argument == "" || len(argument) > 4096 || strings.IndexFunc(argument, unicode.IsControl) >= 0 {
			return "", errors.New("profilehandoff: unsafe argument")
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(argument, "'", `'"'"'`)+"'")
	}
	return strings.Join(quoted, " "), nil
}

func safeBasename(name, suffix string) bool {
	return name != "" && len(name) <= 255 && filepath.Base(name) == name && name != "." && strings.HasSuffix(name, suffix) && strings.IndexFunc(name, unicode.IsControl) < 0
}

func safeToken(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
