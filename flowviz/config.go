// Package flowviz builds bounded, privacy-preserving funnel and transition
// visualizations from pseudonymous sessions and registered route templates.
package flowviz

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ekusiadadus/isutools/internal/safefs"
	"gopkg.in/yaml.v3"
)

const (
	SchemaVersion      = 1
	MaxConfigBytes     = 64 << 10
	MaxConfigNesting   = 32
	MaxStructuralBytes = 4096
	MaxFunnels         = 16
	MaxStepsPerFunnel  = 16
	MaxNodeBytes       = 512
	DefaultMaxNodes    = 16
	DefaultMaxEdges    = 48
	DefaultMaxSessions = 10000
	HardMaxNodes       = 32
	HardMaxEdges       = 128
	HardMaxSessions    = 10000
	ModeOrdered        = "ordered"
)

const (
	EnvMode        = "ISUTOOLS_FLOW_VIZ"
	EnvConfig      = "ISUTOOLS_FUNNEL_CONFIG"
	EnvMaxNodes    = "ISUTOOLS_FLOW_MAX_NODES"
	EnvMaxEdges    = "ISUTOOLS_FLOW_MAX_EDGES"
	StatusReady    = "ready"
	StatusDisabled = "disabled"
	StatusPartial  = "partial"
)

var (
	safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,64}$`)
	routePattern  = regexp.MustCompile(`^[A-Z]{1,16} /[^\s?#]{0,495}$`)
)

type Config struct {
	Version int                `json:"version" yaml:"version"`
	Funnels []FunnelDefinition `json:"funnels,omitempty" yaml:"funnels,omitempty"`
}

type FunnelDefinition struct {
	ID       string           `json:"id" yaml:"id"`
	Scenario string           `json:"scenario" yaml:"scenario"`
	Mode     string           `json:"mode" yaml:"mode"`
	Within   string           `json:"within,omitempty" yaml:"within,omitempty"`
	Steps    []StepDefinition `json:"steps" yaml:"steps"`
}

type StepDefinition struct {
	ID    string `json:"id" yaml:"id"`
	Route string `json:"route" yaml:"route"`
}

type Options struct {
	Enabled     bool
	MaxNodes    int
	MaxEdges    int
	MaxSessions int
	Config      Config
}

func (o Options) normalized() (Options, error) {
	if !o.Enabled {
		return Options{}, nil
	}
	if o.MaxNodes == 0 {
		o.MaxNodes = DefaultMaxNodes
	}
	if o.MaxEdges == 0 {
		o.MaxEdges = DefaultMaxEdges
	}
	if o.MaxSessions == 0 {
		o.MaxSessions = DefaultMaxSessions
	}
	if o.MaxNodes < 2 || o.MaxNodes > HardMaxNodes || o.MaxEdges < 1 || o.MaxEdges > HardMaxEdges ||
		o.MaxSessions < 1 || o.MaxSessions > HardMaxSessions {
		return Options{}, errors.New("flowviz: option-out-of-range")
	}
	if o.Config.Version == 0 && len(o.Config.Funnels) == 0 {
		o.Config.Version = SchemaVersion
	}
	if err := o.Config.Validate(); err != nil {
		return Options{}, err
	}
	return o, nil
}

func (c Config) Validate() error {
	if c.Version != SchemaVersion {
		return errors.New("flowviz: unsupported-version")
	}
	if len(c.Funnels) > MaxFunnels {
		return errors.New("flowviz: too-many-funnels")
	}
	ids := make(map[string]struct{}, len(c.Funnels))
	for _, funnel := range c.Funnels {
		if !safeIDPattern.MatchString(funnel.ID) || !safeIDPattern.MatchString(funnel.Scenario) {
			return errors.New("flowviz: invalid-identifier")
		}
		if _, exists := ids[funnel.ID]; exists {
			return errors.New("flowviz: duplicate-funnel")
		}
		ids[funnel.ID] = struct{}{}
		if funnel.Mode != ModeOrdered {
			return errors.New("flowviz: unsupported-mode")
		}
		if len(funnel.Steps) < 2 || len(funnel.Steps) > MaxStepsPerFunnel {
			return errors.New("flowviz: invalid-step-count")
		}
		if _, err := parseWindow(funnel.Within); err != nil {
			return err
		}
		stepIDs := make(map[string]struct{}, len(funnel.Steps))
		routes := make(map[string]struct{}, len(funnel.Steps))
		for _, step := range funnel.Steps {
			if !safeIDPattern.MatchString(step.ID) {
				return errors.New("flowviz: invalid-step-id")
			}
			if !utf8.ValidString(step.Route) || len(step.Route) > MaxNodeBytes || !routePattern.MatchString(step.Route) {
				return errors.New("flowviz: invalid-route-template")
			}
			if _, exists := stepIDs[step.ID]; exists {
				return errors.New("flowviz: duplicate-step-id")
			}
			if _, exists := routes[step.Route]; exists {
				return errors.New("flowviz: duplicate-step-route")
			}
			stepIDs[step.ID] = struct{}{}
			routes[step.Route] = struct{}{}
		}
	}
	return nil
}

func parseWindow(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 || d > 24*time.Hour {
		return 0, errors.New("flowviz: invalid-window")
	}
	return d, nil
}

// LoadConfig reads one regular, single-link, bounded YAML or JSON file. Error
// messages are stable codes and never include configuration contents.
func LoadConfig(path string) (Config, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return Config{}, errors.New("flowviz: config-path-empty")
	}
	info, err := os.Lstat(clean)
	if err != nil || !info.Mode().IsRegular() {
		return Config{}, errors.New("flowviz: config-not-regular")
	}
	if info.Size() > MaxConfigBytes {
		return Config{}, errors.New("flowviz: config-too-large")
	}
	root, err := safefs.Open(filepath.Dir(clean), safefs.Options{})
	if err != nil {
		return Config{}, errors.New("flowviz: config-root-unavailable")
	}
	defer func() { _ = root.Close() }()
	body, err := root.ReadFile(filepath.Base(clean), MaxConfigBytes)
	if err != nil {
		switch {
		case errors.Is(err, safefs.ErrTooLarge):
			return Config{}, errors.New("flowviz: config-too-large")
		case errors.Is(err, safefs.ErrNotRegular), errors.Is(err, safefs.ErrAmbiguousLink):
			return Config{}, errors.New("flowviz: config-not-regular")
		default:
			return Config{}, errors.New("flowviz: config-read-failed")
		}
	}
	return parseConfig(body)
}

func parseConfig(body []byte) (Config, error) {
	if len(body) > MaxConfigBytes {
		return Config{}, errors.New("flowviz: config-too-large")
	}
	if !safeConfigStructure(body) {
		return Config{}, errors.New("flowviz: invalid-yaml")
	}
	var node yaml.Node
	if err := yaml.Unmarshal(body, &node); err != nil || !validYAMLTree(&node, 0, new(int)) {
		return Config{}, errors.New("flowviz: invalid-yaml")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, errors.New("flowviz: unknown-field-or-invalid-type")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("flowviz: multiple-documents")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	if len(cfg.Funnels) == 0 {
		return Config{}, errors.New("flowviz: no-funnels")
	}
	return cfg, nil
}

// safeConfigStructure bounds work before the YAML parser sees untrusted
// persisted configuration. Quoted JSON/YAML scalars are ignored so ordinary
// route strings do not consume the structural budget.
func safeConfigStructure(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	depth, structural := 0, 0
	lineStart, indent := true, 0
	inSingle, inDouble, escaped := false, false, false
	for i, b := range body {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			return false
		}
		if b == '\r' && (i+1 >= len(body) || body[i+1] != '\n') {
			return false
		}
		if b == '\n' {
			lineStart, indent = true, 0
		} else if lineStart {
			switch b {
			case ' ':
				indent++
				if indent > MaxConfigNesting {
					return false
				}
			case '\r':
			case '\t':
				return false
			default:
				lineStart = false
			}
		}
		if inDouble {
			if escaped {
				escaped = false
				continue
			}
			switch b {
			case '\\':
				escaped = true
			case '"':
				inDouble = false
			}
			continue
		}
		if inSingle {
			if b == '\'' {
				inSingle = false
			}
			continue
		}
		switch b {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '[', '{':
			depth++
			structural++
			if depth > MaxConfigNesting || structural > MaxStructuralBytes {
				return false
			}
		case ']', '}':
			if depth > 0 {
				depth--
			}
			structural++
			if structural > MaxStructuralBytes {
				return false
			}
		}
	}
	return !inSingle && !inDouble && !escaped
}

func validYAMLTree(node *yaml.Node, depth int, count *int) bool {
	if node == nil {
		return true
	}
	*count++
	if depth > MaxConfigNesting || *count > MaxStructuralBytes || node.Kind == yaml.AliasNode || node.Anchor != "" {
		return false
	}
	for _, child := range node.Content {
		if !validYAMLTree(child, depth+1, count) {
			return false
		}
	}
	return true
}
