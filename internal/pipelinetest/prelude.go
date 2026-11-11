package pipelinetest

import (
	"fmt"
	"strings"

	"github.com/grafana/alloy/syntax/scanner"
	builder "github.com/grafana/alloy/syntax/token/builder"
)

// buildSource builds the final Alloy source for a pipeline test. It replaces
// the config's entry and exit components as the test asks, rewrites
// pipelinetest symbolic references, and appends the test sink, mocks and
// generated input sources.
//
// Everything generated goes after the config, so line numbers in errors about
// the config still point at the user's file.
func buildSource(schema TestSchema) (string, error) {
	inputs := collectInputs(schema.Inputs)

	var replacements []sourceReplacement
	for _, in := range inputs {
		if in.replace != "" {
			replacements = append(replacements, sourceReplacement{id: in.replace, signal: in.signal})
		}
	}

	discovery, err := targetLiterals(schema.Inputs.Targets)
	if err != nil {
		return "", err
	}

	overrides, err := overrideExprs(schema.Config.Override)
	if err != nil {
		return "", err
	}

	transformed, err := transformConfig(configName(schema.Config), schema.Config.Source, transformSpec{
		sources:   replacements,
		sinks:     schema.Config.Sinks,
		remove:    schema.Config.Remove,
		discovery: discovery,
		overrides: overrides,
	})
	if err != nil {
		return "", err
	}

	generated, err := buildSources(inputs, transformed, schema.Mocks)
	if err != nil {
		return "", err
	}

	return rewritePipelineTestRefs(transformed.source) + "\n\n" + buildSink() + "\n\n" + generated, nil
}

func configName(cfg ConfigSchema) string {
	if cfg.Path != "" {
		return cfg.Path
	}
	return "config"
}

func rewritePipelineTestRefs(source string) string {
	replacer := strings.NewReplacer(
		"pipelinetest.loki.url", "pipelinetest.sink.out.loki_push_url",
		"pipelinetest.loki.receiver", "pipelinetest.sink.out.loki_receiver",
		"pipelinetest.prometheus.url", "pipelinetest.sink.out.prometheus_remote_write_url",
		"pipelinetest.prometheus.receiver", "pipelinetest.sink.out.prometheus_receiver",
		"pipelinetest.otel.input", "pipelinetest.sink.out.otel_input",
	)
	return replacer.Replace(source)
}

func buildSink() string {
	return `pipelinetest.sink "out" {}`
}

// testInput is one input of any signal that needs a generated source.
type testInput struct {
	signal     signal
	index      int
	components []string
	replace    string
	hasData    bool
}

func collectInputs(inputs InputSchema) []testInput {
	var out []testInput
	for i, in := range inputs.Loki {
		out = append(out, testInput{signal: signalLogs, index: i, components: in.Components, replace: in.Replace, hasData: len(in.Entries) > 0})
	}
	for i, in := range inputs.Prometheus {
		out = append(out, testInput{signal: signalMetrics, index: i, components: in.Components, replace: in.Replace, hasData: len(in.Samples) > 0})
	}
	for i, in := range inputs.Traces {
		out = append(out, testInput{signal: signalTraces, index: i, components: in.Components, replace: in.Replace, hasData: len(in.Spans) > 0})
	}
	return out
}

// buildSources generates the mocks and one pipelinetest.source per input.
func buildSources(inputs []testInput, cfg transformedConfig, mocks MockSchema) (string, error) {
	var out strings.Builder

	file := builder.NewFile()
	appendMocks(file.Body(), mocks)
	if len(file.Body().Nodes()) > 0 {
		out.Write(file.Bytes())
		out.WriteString("\n")
	}

	for _, in := range inputs {
		if len(in.components) > 0 && in.replace != "" {
			return "", fmt.Errorf("%s input %d: set components or replace, not both", in.signal, in.index)
		}

		var forwardTo string
		switch {
		case in.replace != "":
			forwardTo = cfg.sources[in.replace].forwardTo
		case len(in.components) > 0:
			targets := make([]string, 0, len(in.components))
			for _, c := range in.components {
				if !isReference(c) {
					return "", fmt.Errorf("%s input %d: %q is not a component export such as loki.process.default.receiver", in.signal, in.index, c)
				}
				if replacement, ok := cfg.sinkRefs[c]; ok {
					c = replacement
				}
				targets = append(targets, rewritePipelineTestRefs(c))
			}
			forwardTo = "[" + strings.Join(targets, ", ") + "]"
		case in.hasData:
			return "", fmt.Errorf("%s input %d: set components or replace to say where its data goes", in.signal, in.index)
		default:
			continue
		}

		fmt.Fprintf(&out, "pipelinetest.source %q {\n\tforward_to {\n\t\t%s = %s\n\t}\n}\n\n",
			generatedInputComponentName(in.signal, in.index), in.signal, forwardTo)
	}

	return out.String(), nil
}

// isReference reports whether s is a dotted chain of identifiers, so it can be
// pasted into the generated config as is.
func isReference(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if !scanner.IsValidIdentifier(p) {
			return false
		}
	}
	return true
}

func generatedInputComponentID(sig signal, i int) string {
	return "pipelinetest.source." + generatedInputComponentName(sig, i)
}

func generatedInputComponentName(sig signal, i int) string {
	return fmt.Sprintf("__pipelinetest_%s_input_%d", sig, i)
}
