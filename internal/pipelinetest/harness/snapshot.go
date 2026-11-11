package harness

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// snapshotLimit caps how many items of each signal a Snapshot keeps. A real
// pipeline can capture thousands of samples, and a failure report only needs
// enough of them to show what arrived.
const snapshotLimit = 100

// Snapshot is a copy of what the sink captured, in a form fit for reports and
// JSON output. Each signal keeps at most snapshotLimit items, and the totals
// count everything captured.
type Snapshot struct {
	Logs    []LogEntry `json:"logs"`
	Metrics []Sample   `json:"metrics"`
	Spans   []SpanInfo `json:"spans"`

	Totals SnapshotTotals `json:"totals"`
}

// SnapshotTotals counts everything the sink captured.
type SnapshotTotals struct {
	Logs    int `json:"logs"`
	Metrics int `json:"metrics"`
	Spans   int `json:"spans"`
	// OTLP metric data points and log records reach the sink, but cannot be
	// asserted on yet.
	OTLPDataPoints int `json:"otlp_data_points,omitempty"`
	OTLPLogRecords int `json:"otlp_log_records,omitempty"`
}

// LogEntry is one captured Loki entry.
type LogEntry struct {
	Labels             map[string]string `json:"labels"`
	Line               string            `json:"line"`
	Timestamp          time.Time         `json:"timestamp"`
	StructuredMetadata map[string]string `json:"structured_metadata,omitempty"`
}

// Sample is one captured Prometheus sample.
type Sample struct {
	Labels map[string]string `json:"labels"`
	Value  *float64          `json:"value,omitempty"`
	// ValueText holds values JSON cannot encode: NaN, such as a staleness
	// marker, and the infinities.
	ValueText string    `json:"value_text,omitempty"`
	Histogram bool      `json:"histogram,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type,omitempty"`
	Help      string    `json:"help,omitempty"`
	Unit      string    `json:"unit,omitempty"`
}

// SpanInfo is one captured span.
type SpanInfo struct {
	Name               string            `json:"name"`
	Kind               string            `json:"kind"`
	Status             string            `json:"status"`
	Scope              string            `json:"scope,omitempty"`
	TraceID            string            `json:"trace_id,omitempty"`
	SpanID             string            `json:"span_id,omitempty"`
	ParentSpanID       string            `json:"parent_span_id,omitempty"`
	Attributes         map[string]string `json:"attributes,omitempty"`
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
}

// Text renders the snapshot for a terminal, one line per item, with each
// signal capped at limit items.
func (s Snapshot) Text(limit int) string {
	var sections []string
	if s.Totals.Logs > 0 {
		sections = append(sections, renderCapped("logs", s.Totals.Logs, len(s.Logs), limit, func(i int) string {
			e := s.Logs[i]
			return fmt.Sprintf("%s %q", renderStringMap(e.Labels), e.Line)
		}))
	}
	if s.Totals.Metrics > 0 {
		sections = append(sections, renderCapped("metrics", s.Totals.Metrics, len(s.Metrics), limit, func(i int) string {
			m := s.Metrics[i]
			if m.Histogram {
				return renderStringMap(m.Labels) + " histogram"
			}
			if m.Value != nil {
				return renderStringMap(m.Labels) + " " + fmt.Sprint(*m.Value)
			}
			if m.ValueText != "" {
				return renderStringMap(m.Labels) + " " + m.ValueText
			}
			return renderStringMap(m.Labels)
		}))
	}
	if s.Totals.Spans > 0 {
		sections = append(sections, renderCapped("spans", s.Totals.Spans, len(s.Spans), limit, func(i int) string {
			sp := s.Spans[i]
			line := fmt.Sprintf("%q kind=%s status=%s", sp.Name, sp.Kind, sp.Status)
			if len(sp.Attributes) > 0 {
				line += " attributes=" + renderStringMap(sp.Attributes)
			}
			return line
		}))
	}
	if s.Totals.OTLPDataPoints > 0 || s.Totals.OTLPLogRecords > 0 {
		sections = append(sections, fmt.Sprintf("otlp metric data points: %d, otlp log records: %d (not assertable yet)",
			s.Totals.OTLPDataPoints, s.Totals.OTLPLogRecords))
	}
	if len(sections) == 0 {
		return "nothing reached the sink"
	}
	return strings.Join(sections, "\n")
}

func renderCapped(name string, total, kept, limit int, line func(i int) string) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "%s (%d):", name, total)
	n := min(kept, limit)
	for i := range n {
		b.WriteString("\n  ")
		b.WriteString(line(i))
	}
	if total > n {
		_, _ = fmt.Fprintf(&b, "\n  … %d more", total-n)
	}
	return b.String()
}

func exportSnapshot(s snapshot) Snapshot {
	out := Snapshot{
		Logs:    make([]LogEntry, 0, min(len(s.loki), snapshotLimit)),
		Metrics: make([]Sample, 0, min(len(s.prometheus), snapshotLimit)),
		Spans:   make([]SpanInfo, 0, min(len(s.spans), snapshotLimit)),
		Totals: SnapshotTotals{
			Logs:           len(s.loki),
			Metrics:        len(s.prometheus),
			Spans:          len(s.spans),
			OTLPDataPoints: s.otlpDataPoints,
			OTLPLogRecords: s.otlpLogRecords,
		},
	}

	for _, e := range s.loki[:min(len(s.loki), snapshotLimit)] {
		labels := make(map[string]string, len(e.Labels))
		for k, v := range e.Labels {
			labels[string(k)] = string(v)
		}
		var sm map[string]string
		if len(e.StructuredMetadata) > 0 {
			sm = make(map[string]string, len(e.StructuredMetadata))
			for _, l := range e.StructuredMetadata {
				sm[l.Name] = l.Value
			}
		}
		out.Logs = append(out.Logs, LogEntry{
			Labels:             labels,
			Line:               e.Line,
			Timestamp:          e.Timestamp.UTC(),
			StructuredMetadata: sm,
		})
	}

	for _, p := range s.prometheus[:min(len(s.prometheus), snapshotLimit)] {
		sample := Sample{
			Labels:    p.Labels.Map(),
			Histogram: p.Histogram != nil,
			Timestamp: p.Timestamp,
		}
		switch {
		case p.Histogram != nil:
		case math.IsNaN(p.Value) || math.IsInf(p.Value, 0):
			sample.ValueText = fmt.Sprint(p.Value)
		default:
			v := p.Value
			sample.Value = &v
		}
		if p.Metadata != nil {
			sample.Type = string(p.Metadata.Type)
			sample.Help = p.Metadata.Help
			sample.Unit = p.Metadata.Unit
		}
		out.Metrics = append(out.Metrics, sample)
	}

	for _, sp := range s.spans[:min(len(s.spans), snapshotLimit)] {
		out.Spans = append(out.Spans, SpanInfo{
			Name:               sp.Name,
			Kind:               sp.Kind,
			Status:             sp.StatusCode,
			Scope:              sp.Scope,
			TraceID:            sp.TraceID,
			SpanID:             sp.SpanID,
			ParentSpanID:       sp.ParentSpanID,
			Attributes:         sp.Attributes,
			ResourceAttributes: sp.ResourceAttributes,
		})
	}

	return out
}
