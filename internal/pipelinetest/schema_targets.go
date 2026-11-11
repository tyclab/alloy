package pipelinetest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/grafana/alloy/internal/pipelinetest/harness"
	builder "github.com/grafana/alloy/syntax/token/builder"
)

// TargetInputSchema replaces a discovery component's targets export with a
// fixed list, so the relabel rules after it run on targets the test controls.
// Include the __meta_ labels the rules read.
type TargetInputSchema struct {
	Replace string              `yaml:"replace"`
	Targets []map[string]string `yaml:"targets"`
}

// TargetAssertionSchema describes one assertion about the targets a component
// exports, such as discovery.relabel.pods.output.
type TargetAssertionSchema struct {
	Name string `yaml:"name,omitempty"`
	// From is the component export to read, such as
	// discovery.relabel.pods.output.
	From  string            `yaml:"from"`
	Type  string            `yaml:"type,omitempty"`
	Count *int              `yaml:"count,omitempty"`
	Match TargetMatchSchema `yaml:"match,omitempty"`
}

// TargetMatchSchema matches a target's labels. Labels match partially by
// default, because targets carry many labels the test does not care about,
// such as the __meta_ labels discovery adds.
type TargetMatchSchema struct {
	Selector string         `yaml:"selector,omitempty"`
	Labels   MapMatchSchema `yaml:"labels,omitempty"`
}

// targetLiterals renders each targets input as the Alloy list literal that
// replaces references to its component's targets.
func targetLiterals(inputs []TargetInputSchema) (map[string]string, error) {
	out := make(map[string]string, len(inputs))
	for i, in := range inputs {
		if in.Replace == "" {
			return nil, fmt.Errorf("targets input %d: replace is required", i)
		}
		if !strings.HasPrefix(in.Replace, "discovery.") {
			return nil, fmt.Errorf("targets input %d: only discovery components can be replaced with targets, got %s", i, in.Replace)
		}
		targets := in.Targets
		if targets == nil {
			targets = []map[string]string{}
		}
		out[in.Replace] = alloyLiteral(targets)
	}
	return out, nil
}

// alloyLiteral renders a value decoded from YAML as a one-line Alloy literal.
func alloyLiteral(v any) string {
	expr := builder.NewExpr()
	expr.SetValue(v)
	// The builder writes one field per line. Raw newlines only separate
	// tokens, as string values escape theirs, so joining the lines keeps the
	// literal valid and every later line number of the config intact.
	return newlineAndIndent.ReplaceAllString(string(expr.Bytes()), " ")
}

// overrideExprs renders the values of config.override as Alloy literals.
func overrideExprs(overrides map[string]map[string]any) (map[string]map[string]string, error) {
	out := make(map[string]map[string]string, len(overrides))
	for id, attrs := range overrides {
		if !isReference(id) {
			return nil, fmt.Errorf("override: %q is not a component ID", id)
		}
		out[id] = make(map[string]string, len(attrs))
		for name, v := range attrs {
			out[id][name] = alloyLiteral(v)
		}
	}
	return out, nil
}

var newlineAndIndent = regexp.MustCompile(`\n[ \t]*`)

func targetRefs(assertions []TargetAssertionSchema) []string {
	refs := make([]string, 0, len(assertions))
	for _, a := range assertions {
		refs = append(refs, a.From)
	}
	return refs
}

func buildTargetAssertions(assertions []TargetAssertionSchema) ([]harness.Assertion, error) {
	out := make([]harness.Assertion, 0, len(assertions))
	for i, a := range assertions {
		if a.From == "" || !isReference(a.From) {
			return nil, fmt.Errorf("targets assertion %d: from must be a component export such as discovery.relabel.pods.output", i)
		}

		var matchers []harness.TargetMatcher
		if a.Match.Selector != "" {
			selector, err := harness.ParseSelector(a.Match.Selector)
			if err != nil {
				return nil, fmt.Errorf("targets assertion %d: %w", i, err)
			}
			matchers = append(matchers, harness.TargetSelector(a.Match.Selector, selector))
		}
		if len(a.Match.Labels.Values) > 0 {
			partial, err := isPartialMapMatchDefault("labels", a.Match.Labels, true)
			if err != nil {
				return nil, fmt.Errorf("targets assertion %d: %w", i, err)
			}
			matchers = append(matchers, harness.TargetLabels(a.Match.Labels.Values, partial))
		}

		switch assertionType(a.Type, a.Count) {
		case assertionTypeCount:
			if a.Count == nil {
				return nil, fmt.Errorf("targets assertion %d: count requires count", i)
			}
			out = append(out, harness.TargetCount(a.From, *a.Count, matchers...))
		case assertionTypeContains:
			if len(matchers) == 0 {
				return nil, errors.New("contains requires at least one match field")
			}
			out = append(out, harness.TargetContains(a.From, matchers...))
		default:
			return nil, fmt.Errorf("unknown assertion type %q", a.Type)
		}
	}
	return out, nil
}

func (a TargetAssertionSchema) describe() string {
	return describeMatch(a.Count, a.From, a.Match.Selector, describeMap("labels", a.Match.Labels))
}
