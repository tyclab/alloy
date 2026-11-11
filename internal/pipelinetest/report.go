package pipelinetest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/grafana/alloy/internal/pipelinetest/harness"
)

// Status is the outcome of one test file.
type Status string

const (
	StatusPassed Status = "passed"
	StatusFailed Status = "failed"
	// StatusError means the test could not run, for example because the
	// config does not load. No assertion was evaluated.
	StatusError Status = "error"
)

// Report is the outcome of a run over several test files. It is also the JSON
// document written by WriteJSON, so its shape is part of the CLI's contract.
type Report struct {
	Tests   []FileReport `json:"tests"`
	Summary Summary      `json:"summary"`
}

// Summary counts test files by outcome.
type Summary struct {
	Total           int     `json:"total"`
	Passed          int     `json:"passed"`
	Failed          int     `json:"failed"`
	Errors          int     `json:"errors"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// FileReport is the outcome of one test file.
type FileReport struct {
	File            string  `json:"file"`
	Status          Status  `json:"status"`
	DurationSeconds float64 `json:"duration_seconds"`
	// Error is set when Status is error.
	Error      string            `json:"error,omitempty"`
	Assertions []AssertionReport `json:"assertions,omitempty"`
	// UnhealthyComponents and Snapshot are set when the test failed, to help
	// explain why.
	UnhealthyComponents []harness.ComponentHealth `json:"unhealthy_components,omitempty"`
	Snapshot            *harness.Snapshot         `json:"snapshot,omitempty"`
}

// AssertionReport is the outcome of one assertion.
type AssertionReport struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Message string `json:"message,omitempty"`
}

// Passed reports whether every test passed.
func (r Report) Passed() bool {
	return r.Summary.Passed == r.Summary.Total
}

// FindTestFiles expands paths into test files. A file is used as is. A
// directory is searched recursively for files named test.yaml or ending in
// .test.yaml. The result is sorted and has no duplicates.
func FindTestFiles(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, filepath.Clean(p))
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && IsTestFileName(d.Name()) {
				files = append(files, filepath.Clean(path))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

// IsTestFileName reports whether a file name follows the test file naming
// convention: test.yaml, or a name ending in .test.yaml.
func IsTestFileName(name string) bool {
	return name == "test.yaml" || strings.HasSuffix(name, ".test.yaml")
}

// RunOptions configures RunFiles.
type RunOptions struct {
	AssertTimeout time.Duration
	// LogWriter receives Alloy's logs. They are discarded when nil.
	LogWriter io.Writer
	// OnResult, when set, is called after each test file finishes, so a
	// reporter can stream results.
	OnResult func(FileReport)
}

// RunFiles runs each test file in turn. Tests cannot run in parallel, because
// the harness uses the default Prometheus registry.
func RunFiles(ctx context.Context, files []string, opts RunOptions) Report {
	start := time.Now()
	report := Report{Tests: make([]FileReport, 0, len(files))}

	for _, file := range files {
		fr := runFile(ctx, file, opts)
		report.Tests = append(report.Tests, fr)
		switch fr.Status {
		case StatusPassed:
			report.Summary.Passed++
		case StatusFailed:
			report.Summary.Failed++
		default:
			report.Summary.Errors++
		}
		if opts.OnResult != nil {
			opts.OnResult(fr)
		}
	}

	report.Summary.Total = len(files)
	report.Summary.DurationSeconds = roundSeconds(time.Since(start))
	return report
}

func runFile(ctx context.Context, file string, opts RunOptions) (fr FileReport) {
	start := time.Now()
	fr = FileReport{File: file}
	defer func() { fr.DurationSeconds = roundSeconds(time.Since(start)) }()

	dataPath, err := os.MkdirTemp("", "alloy-pipelinetest-")
	if err != nil {
		fr.Status, fr.Error = StatusError, err.Error()
		return fr
	}
	defer func() { _ = os.RemoveAll(dataPath) }()

	result, err := RunTestFile(ctx, file, TestConfig{
		DataPath:      dataPath,
		AssertTimeout: opts.AssertTimeout,
		LogWriter:     opts.LogWriter,
	})
	if err != nil {
		fr.Status, fr.Error = StatusError, err.Error()
		return fr
	}

	for _, c := range result.Checks {
		ar := AssertionReport{Name: c.Name, Passed: c.Err == nil}
		if c.Err != nil {
			ar.Message = c.Err.Error()
		}
		fr.Assertions = append(fr.Assertions, ar)
	}

	fr.Status = StatusPassed
	if !result.Passed() {
		fr.Status = StatusFailed
		fr.UnhealthyComponents = result.Unhealthy
		fr.Snapshot = &result.Snapshot
	}
	return fr
}

func roundSeconds(d time.Duration) float64 {
	return float64(d.Round(time.Millisecond)) / float64(time.Second)
}

// WriteJSON writes the report as one indented JSON document.
func WriteJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// TextReporter writes results for a terminal, as they arrive.
type TextReporter struct {
	W     io.Writer
	Color bool
	// SnapshotLimit caps how many captured items of each signal a failure
	// shows. Defaults to 20.
	SnapshotLimit int
}

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
)

func (t TextReporter) paint(code, s string) string {
	if !t.Color {
		return s
	}
	return code + s + ansiReset
}

// File writes the result of one test file.
func (t TextReporter) File(fr FileReport) {
	duration := t.paint(ansiDim, fmt.Sprintf("(%.2fs)", fr.DurationSeconds))
	switch fr.Status {
	case StatusPassed:
		t.printf("%s %s %s\n", t.paint(ansiGreen+ansiBold, "PASS "), fr.File, duration)
	case StatusFailed:
		t.printf("%s %s %s\n", t.paint(ansiRed+ansiBold, "FAIL "), fr.File, duration)
	default:
		t.printf("%s %s %s\n", t.paint(ansiYellow+ansiBold, "ERROR"), fr.File, duration)
		t.indented(4, t.paint(ansiYellow, fr.Error))
		return
	}

	for _, a := range fr.Assertions {
		if a.Passed {
			t.printf("    %s %s\n", t.paint(ansiGreen, "✓"), a.Name)
			continue
		}
		t.printf("    %s %s\n", t.paint(ansiRed, "✗"), t.paint(ansiRed, a.Name))
		t.indented(8, a.Message)
	}

	if fr.Status != StatusFailed {
		return
	}
	if len(fr.UnhealthyComponents) > 0 {
		t.printf("    %s\n", t.paint(ansiYellow, "unhealthy components:"))
		for _, u := range fr.UnhealthyComponents {
			t.indented(8, fmt.Sprintf("%s (%s): %s", u.ID, u.Health, u.Message))
		}
	}
	if fr.Snapshot != nil {
		limit := t.SnapshotLimit
		if limit <= 0 {
			limit = 20
		}
		t.printf("    %s\n", t.paint(ansiDim, "what reached the sink:"))
		t.indented(8, t.paint(ansiDim, fr.Snapshot.Text(limit)))
	}
}

// Summary writes the closing line of a run.
func (t TextReporter) Summary(r Report) {
	s := r.Summary
	parts := []string{t.paint(ansiGreen, fmt.Sprintf("%d passed", s.Passed))}
	if s.Failed > 0 {
		parts = append(parts, t.paint(ansiRed, fmt.Sprintf("%d failed", s.Failed)))
	}
	if s.Errors > 0 {
		parts = append(parts, t.paint(ansiYellow, fmt.Sprintf("%d errors", s.Errors)))
	}
	noun := "tests"
	if s.Total == 1 {
		noun = "test"
	}
	t.printf("\n%d %s: %s %s\n", s.Total, noun, strings.Join(parts, ", "),
		t.paint(ansiDim, fmt.Sprintf("(%.2fs)", s.DurationSeconds)))
}

func (t TextReporter) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(t.W, format, args...)
}

func (t TextReporter) indented(n int, text string) {
	pad := strings.Repeat(" ", n)
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		t.printf("%s%s\n", pad, line)
	}
}
