package pipelinetest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const reportConfig = `
loki.process "default" {
  stage.static_labels {
    values = {env = "test"}
  }
  forward_to = [loki.write.default.receiver]
}

loki.write "default" {
  endpoint {
    url = "http://example.com/loki/api/v1/push"
  }
}
`

func TestRunFilesReport(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.alloy"), reportConfig)

	testFile := func(configPath, want string) string {
		return `
config:
  path: ` + configPath + `
  sinks: [loki.write.default]
inputs:
  loki:
    - components: [loki.process.default.receiver]
      entries:
        - line: hello
assert:
  loki:
    - name: env label added
      match:
        selector: '{env="` + want + `"}'
`
	}
	writeFile(t, filepath.Join(dir, "pass.test.yaml"), testFile("config.alloy", "test"))
	writeFile(t, filepath.Join(dir, "sub", "fail.test.yaml"), testFile("../config.alloy", "prod"))
	writeFile(t, filepath.Join(dir, "sub", "test.yaml"), "config: 'not alloy {'\nassert:\n  loki:\n    - count: 1\n")
	writeFile(t, filepath.Join(dir, "ignored.yaml"), "not a test")

	files, err := FindTestFiles([]string{dir})
	require.NoError(t, err)
	require.Equal(t, []string{
		filepath.Join(dir, "pass.test.yaml"),
		filepath.Join(dir, "sub", "fail.test.yaml"),
		filepath.Join(dir, "sub", "test.yaml"),
	}, files)

	var streamed []string
	report := RunFiles(t.Context(), files, RunOptions{
		AssertTimeout: time.Second,
		OnResult:      func(fr FileReport) { streamed = append(streamed, string(fr.Status)) },
	})

	require.Equal(t, []string{"passed", "failed", "error"}, streamed)
	require.Equal(t, Summary{Total: 3, Passed: 1, Failed: 1, Errors: 1, DurationSeconds: report.Summary.DurationSeconds}, report.Summary)
	require.False(t, report.Passed())

	failed := report.Tests[1]
	require.Equal(t, "loki[0] contains: env label added", failed.Assertions[0].Name)
	require.Contains(t, failed.Assertions[0].Message, `no matching entry found for selector {env="prod"}`)
	require.NotNil(t, failed.Snapshot)
	require.Equal(t, map[string]string{"env": "test"}, failed.Snapshot.Logs[0].Labels)

	require.Nil(t, report.Tests[0].Snapshot, "passing tests carry no snapshot")
	require.Contains(t, report.Tests[2].Error, "config:1")

	var buf bytes.Buffer
	require.NoError(t, WriteJSON(&buf, report))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Contains(t, decoded, "summary")

	var text bytes.Buffer
	r := TextReporter{W: &text}
	for _, fr := range report.Tests {
		r.File(fr)
	}
	r.Summary(report)
	require.Contains(t, text.String(), "✗ loki[0] contains: env label added")
	require.Contains(t, text.String(), "3 tests: 1 passed, 1 failed, 1 errors")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
