package pipelinetest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/grafana/alloy/internal/pipelinetest/harness"
)

const (
	mapMatchModeExact   = "exact"
	mapMatchModePartial = "partial"

	assertionTypeCount    = "count"
	assertionTypeContains = "contains"
)

// TestSchema describes a declarative pipeline test loaded from a text file.
type TestSchema struct {
	Flags  FlagsSchema     `yaml:"flags,omitempty"`
	Config ConfigSchema    `yaml:"config"`
	Inputs InputSchema     `yaml:"inputs"`
	Mocks  MockSchema      `yaml:"mocks"`
	Assert AssertionSchema `yaml:"assert"`
}

// FlagsSchema holds the Alloy run flags enabled for a pipeline test.
type FlagsSchema struct {
	LokiPipelineV2 bool `yaml:"feature.loki.pipeline-v2.enabled,omitempty"`
}

// ConfigSchema is the Alloy configuration under test. It is either inline
// Alloy source, or a mapping that points at a file and lists the components to
// swap for the test harness:
//
//	config:
//	  path: ./config.alloy
//	  sinks: [loki.write.default]
//	  remove: [discovery.kubernetes.pods]
//
// Entry components are replaced by the inputs that name them with replace.
type ConfigSchema struct {
	// Source is the Alloy source. It is set directly for inline configs, and
	// read from Path otherwise.
	Source string `yaml:"-"`
	// Path is the Alloy file to test. A relative path is resolved against the
	// directory of the test file.
	Path string `yaml:"path,omitempty"`
	// Sinks lists exit components, such as loki.write.default, to replace with
	// the test sink. Every reference to their receiver is rewritten to the
	// sink's, so the data they would have sent is captured for assertions.
	Sinks []string `yaml:"sinks,omitempty"`
	// Remove lists components to delete from the config, such as discovery
	// components that need a real environment. Nothing may reference them.
	Remove []string `yaml:"remove,omitempty"`
}

func (c *ConfigSchema) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		*c = ConfigSchema{Source: value.Value}
		return nil
	case yaml.MappingNode:
		type plain ConfigSchema
		var cfg plain
		if err := value.Decode(&cfg); err != nil {
			return err
		}
		if cfg.Path == "" {
			return errors.New("config mapping requires path")
		}
		*c = ConfigSchema(cfg)
		return nil
	default:
		return fmt.Errorf("config must be inline Alloy source or a mapping with a path")
	}
}

// resolve reads the config file, resolving a relative Path against baseDir.
func (c *ConfigSchema) resolve(baseDir string) error {
	if c.Path == "" {
		return nil
	}
	path := c.Path
	if !filepath.IsAbs(path) && baseDir != "" {
		path = filepath.Join(baseDir, path)
	}
	bb, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config path %q: %w", c.Path, err)
	}
	c.Path = path
	c.Source = string(bb)
	return nil
}

// InputSchema groups pipeline test inputs by signal type.
type InputSchema struct {
	Loki       []LokiInputSchema       `yaml:"loki"`
	Prometheus []PrometheusInputSchema `yaml:"prometheus"`
	Traces     []TraceInputSchema      `yaml:"traces"`
	HTTP       []HTTPInputSchema       `yaml:"http"`
	TCP        []TCPInputSchema        `yaml:"tcp"`
	UDP        []UDPInputSchema        `yaml:"udp"`
}

// AssertionSchema groups declarative assertions by signal type.
type AssertionSchema struct {
	Loki       []LokiAssertionSchema       `yaml:"loki"`
	Prometheus []PrometheusAssertionSchema `yaml:"prometheus"`
	Traces     []TraceAssertionSchema      `yaml:"traces"`
}

// MapMatchSchema describes map-like fields such as labels or structured
// metadata. Mode controls whether Values must match exactly or be contained in
// the actual field.
//
// It accepts two forms. The full form sets the mode:
//
//	labels:
//	  mode: partial
//	  values: {job: api}
//
// The short form is a plain map, and uses the field's default mode:
//
//	labels: {job: api}
//
// A mapping is read as the full form when its keys are only mode and values,
// and values, if set, is itself a mapping.
type MapMatchSchema struct {
	Mode   string            `yaml:"mode,omitempty"`
	Values map[string]string `yaml:"values,omitempty"`
}

func (m *MapMatchSchema) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping of names to values", value.Line)
	}

	if isFullMapMatch(value) {
		type plain MapMatchSchema
		var full plain
		if err := value.Decode(&full); err != nil {
			return err
		}
		*m = MapMatchSchema(full)
		return nil
	}

	var values map[string]string
	if err := value.Decode(&values); err != nil {
		return err
	}
	*m = MapMatchSchema{Values: values}
	return nil
}

func isFullMapMatch(value *yaml.Node) bool {
	if len(value.Content) == 0 {
		return false
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key, val := value.Content[i].Value, value.Content[i+1]
		switch key {
		case "mode":
			if val.Kind != yaml.ScalarNode {
				return false
			}
		case "values":
			if val.Kind != yaml.MappingNode {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// produceInputs sends all configured test inputs into the running pipeline,
// in this order: Loki entries, Prometheus samples, spans, HTTP requests, TCP
// data, then UDP datagrams.
func produceInputs(ctx context.Context, alloy *harness.Alloy, inputs InputSchema) error {
	if err := produceLokiInputs(ctx, alloy, inputs.Loki); err != nil {
		return err
	}
	if err := producePrometheusInputs(ctx, alloy, inputs.Prometheus); err != nil {
		return err
	}
	if err := produceTraceInputs(ctx, alloy, inputs.Traces); err != nil {
		return err
	}
	if err := produceHTTPInputs(ctx, inputs.HTTP); err != nil {
		return err
	}
	if err := produceTCPInputs(ctx, inputs.TCP); err != nil {
		return err
	}
	return produceUDPInputs(ctx, alloy, inputs.UDP)
}

// buildChecks builds every assertion with a name for reports, such as
// "loki[0] contains".
func buildChecks(assertions AssertionSchema) ([]harness.Check, error) {
	var checks []harness.Check

	lokiAssertions, err := buildLokiAssertions(assertions.Loki)
	if err != nil {
		return nil, err
	}
	for i, a := range lokiAssertions {
		checks = append(checks, harness.Check{Name: checkName("loki", i, assertions.Loki[i].Type, assertions.Loki[i].Count, assertions.Loki[i].Name, assertions.Loki[i].describe()), Assertion: a})
	}

	promAssertions, err := buildPrometheusAssertions(assertions.Prometheus)
	if err != nil {
		return nil, err
	}
	for i, a := range promAssertions {
		checks = append(checks, harness.Check{Name: checkName("prometheus", i, assertions.Prometheus[i].Type, assertions.Prometheus[i].Count, assertions.Prometheus[i].Name, assertions.Prometheus[i].describe()), Assertion: a})
	}

	traceAssertions, err := buildTraceAssertions(assertions.Traces)
	if err != nil {
		return nil, err
	}
	for i, a := range traceAssertions {
		checks = append(checks, harness.Check{Name: checkName("traces", i, assertions.Traces[i].Type, assertions.Traces[i].Count, assertions.Traces[i].Name, assertions.Traces[i].describe()), Assertion: a})
	}

	return checks, nil
}

// assertionType returns the assertion type, inferring it when unset: an
// assertion with a count is a count assertion, and any other is contains.
func assertionType(typ string, count *int) string {
	if typ != "" {
		return typ
	}
	if count != nil {
		return assertionTypeCount
	}
	return assertionTypeContains
}

// checkName names an assertion in reports: its signal, index and type, then
// its name, or a description of what it matches when it has none.
func checkName(sig string, i int, typ string, count *int, name, description string) string {
	label := fmt.Sprintf("%s[%d] %s", sig, i, assertionType(typ, count))
	switch {
	case name != "":
		label += ": " + name
	case description != "":
		label += ": " + description
	}
	return label
}

func isPartialMapMatch(name string, match MapMatchSchema) (bool, error) {
	return isPartialMapMatchDefault(name, match, false)
}

// isPartialMapMatchDefault is isPartialMapMatch with the mode used when none
// is set.
func isPartialMapMatchDefault(name string, match MapMatchSchema, defaultPartial bool) (bool, error) {
	switch match.Mode {
	case "":
		return defaultPartial, nil
	case mapMatchModeExact:
		return false, nil
	case mapMatchModePartial:
		return true, nil
	default:
		return false, fmt.Errorf("%s mode must be %q or %q, got %q", name, mapMatchModeExact, mapMatchModePartial, match.Mode)
	}
}

// describeMatch builds a short description of an assertion's match fields,
// used as its report name when the test gives it none.
func describeMatch(count *int, parts ...string) string {
	var kept []string
	if count != nil {
		kept = append(kept, fmt.Sprintf("count = %d", *count))
	}
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	desc := strings.Join(kept, ", ")
	const maxLen = 120
	if len(desc) > maxLen {
		desc = desc[:maxLen-1] + "…"
	}
	return desc
}

func describeMap(name string, m MapMatchSchema) string {
	if len(m.Values) == 0 {
		return ""
	}
	keys := slices.Sorted(maps.Keys(m.Values))
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%q", k, m.Values[k]))
	}
	return name + " {" + strings.Join(pairs, ", ") + "}"
}

func describeString(name, v string) string {
	if v == "" {
		return ""
	}
	return fmt.Sprintf("%s %q", name, v)
}

func (a LokiAssertionSchema) describe() string {
	m := a.Match
	return describeMatch(a.Count, m.Selector, describeMap("labels", m.Labels), describeString("line", m.Line),
		describeString("timestamp", m.Timestamp), describeMap("structured_metadata", m.StructuredMetadata))
}

func (a PrometheusAssertionSchema) describe() string {
	m := a.Match
	var value, histogram string
	if m.Value != nil {
		value = fmt.Sprintf("value = %v", *m.Value)
	}
	if m.Histogram != nil {
		histogram = fmt.Sprintf("histogram = %v", *m.Histogram)
	}
	return describeMatch(a.Count, m.Selector, describeMap("labels", m.Labels), value, histogram, describeString("timestamp", m.Timestamp))
}

func (a TraceAssertionSchema) describe() string {
	m := a.Match
	return describeMatch(a.Count, describeString("name", m.Name), describeString("service_name", m.ServiceName),
		describeString("kind", m.Kind), describeString("status", m.Status), describeString("scope", m.Scope),
		describeMap("attributes", m.Attributes), describeMap("resource_attributes", m.ResourceAttributes))
}
