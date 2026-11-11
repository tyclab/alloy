package pipelinetest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestMapMatchSchemaForms(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want MapMatchSchema
	}{
		{
			name: "full form",
			yaml: "mode: partial\nvalues: {job: api}",
			want: MapMatchSchema{Mode: "partial", Values: map[string]string{"job": "api"}},
		},
		{
			name: "full form without mode",
			yaml: "values: {job: api}",
			want: MapMatchSchema{Values: map[string]string{"job": "api"}},
		},
		{
			name: "short form",
			yaml: "{job: api, level: error}",
			want: MapMatchSchema{Values: map[string]string{"job": "api", "level": "error"}},
		},
		{
			// A label called values holding a string is a short form.
			name: "short form with a label named values",
			yaml: "{values: x, mode: y}",
			want: MapMatchSchema{Values: map[string]string{"values": "x", "mode": "y"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got MapMatchSchema
			require.NoError(t, yaml.Unmarshal([]byte(tt.yaml), &got))
			require.Equal(t, tt.want, got)
		})
	}
}

func TestAssertionTypeInference(t *testing.T) {
	one := 1
	require.Equal(t, "count", assertionType("", &one))
	require.Equal(t, "contains", assertionType("", nil))
	require.Equal(t, "contains", assertionType("contains", &one))
}

func TestSelectorAssertions(t *testing.T) {
	var schema AssertionSchema
	require.NoError(t, yaml.Unmarshal([]byte(`
loki:
  - match: {selector: '{service_name="api", level=~"warn|error"}'}
prometheus:
  - count: 2
    match: {selector: 'up{job!="ignored"}'}
`), &schema))

	checks, err := buildChecks(schema)
	require.NoError(t, err)
	require.Len(t, checks, 2)
	require.Equal(t, `loki[0] contains: {service_name="api", level=~"warn|error"}`, checks[0].Name)
	require.Equal(t, `prometheus[0] count: count = 2, up{job!="ignored"}`, checks[1].Name)
}

func TestSelectorRejectsInvalidSyntax(t *testing.T) {
	_, err := buildChecks(AssertionSchema{
		Prometheus: []PrometheusAssertionSchema{{Match: PrometheusMatchSchema{Selector: "up{job="}}},
	})
	require.ErrorContains(t, err, `parse selector "up{job="`)
}

func TestLoadTestFileRejectsUnknownFields(t *testing.T) {
	path := t.TempDir() + "/test.yaml"
	writeFile(t, path, "config: 'x'\nasert: {}\n")
	_, err := LoadTestFile(path)
	require.ErrorContains(t, err, "field asert not found")
}

func TestTargetLiteralsKeepValuesAndLines(t *testing.T) {
	out, err := targetLiterals([]TargetInputSchema{{
		Replace: "discovery.kubernetes.pods",
		Targets: []map[string]string{{"__address__": "a:1", "note": "two  spaces\nand a newline"}},
	}})
	require.NoError(t, err)
	lit := out["discovery.kubernetes.pods"]
	require.NotContains(t, lit, "\n")
	require.Contains(t, lit, `"two  spaces\nand a newline"`)
}
