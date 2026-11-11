package alloycli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPipelineTestCommand(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	write("config.alloy", `
loki.write "default" {
  endpoint {
    url = "http://example.com/loki/api/v1/push"
  }
}
`)
	test := func(line string) string {
		return `
config:
  path: config.alloy
  sinks: [loki.write.default]
inputs:
  loki:
    - components: [loki.write.default.receiver]
      entries:
        - line: hello
assert:
  loki:
    - match: {line: ` + line + `}
`
	}
	write("ok.test.yaml", test("hello"))

	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := pipelineTestCommand()
		cmd.SetArgs(args)
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		err := cmd.Execute()
		return out.String(), err
	}

	out, err := run("--output", "json", dir)
	require.NoError(t, err)
	var report struct {
		Summary struct{ Total, Passed int }
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report), out)
	require.Equal(t, 1, report.Summary.Total)
	require.Equal(t, 1, report.Summary.Passed)

	write("bad.test.yaml", test("bye"))
	out, err = run("--color", "never", "--timeout", "500ms", dir)
	require.ErrorIs(t, err, errTestsFailed)
	require.Contains(t, out, "FAIL  "+filepath.Join(dir, "bad.test.yaml"))
	require.Contains(t, out, "PASS  "+filepath.Join(dir, "ok.test.yaml"))

	_, err = run("--output", "xml", dir)
	require.ErrorContains(t, err, "--output must be text or json")

	_, err = run(t.TempDir())
	require.ErrorContains(t, err, "no test files found")
}
