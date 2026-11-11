package pipelinetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/grafana/alloy/internal/pipelinetest/harness"
)

type TestConfig struct {
	DataPath string
	// BaseDir resolves a relative config path. It is the test file's
	// directory when the test is loaded with LoadTestFile.
	BaseDir string
	// AssertTimeout bounds how long assertions are retried. Defaults to 30s.
	AssertTimeout time.Duration
	// LogWriter receives Alloy's logs. They are discarded when nil.
	LogWriter io.Writer
}

// Result is the outcome of a test that loaded and ran. Checks holds one entry
// per assertion, in the order of the test file.
type Result struct {
	Checks []harness.CheckResult
	// Snapshot is what reached the sink, as of the final assertion attempt.
	Snapshot harness.Snapshot
	// Unhealthy lists components that did not report healthy when the test
	// finished. It often explains why an assertion failed.
	Unhealthy []harness.ComponentHealth
}

// Passed reports whether every check passed.
func (r Result) Passed() bool {
	for _, c := range r.Checks {
		if c.Err != nil {
			return false
		}
	}
	return true
}

// LoadTestFile reads a test file. Unknown fields are rejected, so a typo fails
// the test instead of being silently ignored.
func LoadTestFile(path string) (TestSchema, error) {
	bb, err := os.ReadFile(path)
	if err != nil {
		return TestSchema{}, err
	}

	var schema TestSchema
	dec := yaml.NewDecoder(bytes.NewReader(bb))
	dec.KnownFields(true)
	if err := dec.Decode(&schema); err != nil {
		if errors.Is(err, io.EOF) {
			return TestSchema{}, errors.New("test file is empty")
		}
		return TestSchema{}, err
	}
	return schema, nil
}

// Run runs one test. It returns an error when the test cannot run at all, for
// example because the config does not load or an input cannot be sent, and a
// Result with one entry per assertion otherwise.
func Run(ctx context.Context, schema TestSchema, cfg TestConfig) (Result, error) {
	if err := schema.Config.resolve(cfg.BaseDir); err != nil {
		return Result{}, err
	}
	if err := validateMocks(schema.Mocks); err != nil {
		return Result{}, err
	}
	if err := validateHTTPInputs(schema.Inputs.HTTP); err != nil {
		return Result{}, err
	}
	if err := validateSocketInputs(schema.Inputs.TCP, schema.Inputs.UDP); err != nil {
		return Result{}, err
	}

	checks, err := buildChecks(schema.Assert)
	if err != nil {
		return Result{}, err
	}
	if len(checks) == 0 {
		return Result{}, errors.New("the test has no assertions")
	}

	source, err := buildSource(schema)
	if err != nil {
		return Result{}, err
	}

	alloy, err := harness.NewAlloy(harness.Config{
		SinkID:         "pipelinetest.sink.out",
		DataPath:       cfg.DataPath,
		Source:         source,
		SourceName:     configName(schema.Config),
		LokiPipelineV2: schema.Flags.LokiPipelineV2,
		AssertTimeout:  cfg.AssertTimeout,
		LogWriter:      cfg.LogWriter,
	})
	if err != nil {
		return Result{}, err
	}
	defer alloy.Stop()

	if err := produceInputs(ctx, alloy, schema.Inputs); err != nil {
		return Result{}, err
	}

	alloy.ObserveTargets(targetRefs(schema.Assert.Targets)...)
	results, snapshot := alloy.RunChecks(checks)
	return Result{
		Checks:    results,
		Snapshot:  snapshot,
		Unhealthy: alloy.UnhealthyComponents(),
	}, nil
}

// RunTest runs one test and returns an error describing every failed
// assertion and what reached the sink.
func RunTest(ctx context.Context, schema TestSchema, cfg TestConfig) error {
	result, err := Run(ctx, schema, cfg)
	if err != nil {
		return err
	}
	if result.Passed() {
		return nil
	}

	var b strings.Builder
	b.WriteString("pipeline test failed\n\n")
	for _, c := range result.Checks {
		if c.Err != nil {
			fmt.Fprintf(&b, "- %s: %s\n", c.Name, c.Err)
		}
	}
	for _, u := range result.Unhealthy {
		fmt.Fprintf(&b, "\nunhealthy component %s (%s): %s", u.ID, u.Health, u.Message)
	}
	b.WriteString("\nlatest snapshot:\n")
	b.WriteString(result.Snapshot.Text(50))
	return errors.New(b.String())
}

// RunTestFile loads and runs the test file at path, resolving a relative
// config path against the file's directory.
func RunTestFile(ctx context.Context, path string, cfg TestConfig) (Result, error) {
	schema, err := LoadTestFile(path)
	if err != nil {
		return Result{}, err
	}
	if cfg.BaseDir == "" {
		cfg.BaseDir = filepath.Dir(path)
	}
	return Run(ctx, schema, cfg)
}
