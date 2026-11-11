package harness

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	promql_parser "github.com/prometheus/prometheus/promql/parser"

	"github.com/grafana/alloy/internal/component/common/loki"
)

// ParseSelector parses a PromQL-style series selector, such as
//
//	probe_success{job="blackbox", instance=~"prod-.*"}
//
// or, for Loki streams, a stream selector such as {service_name="api"}. It
// supports the =, !=, =~ and !~ operators. A selector matches when every
// matcher matches, so the series may carry labels the selector does not name.
func ParseSelector(selector string) ([]*labels.Matcher, error) {
	matchers, err := promql_parser.NewParser(promql_parser.Options{}).ParseMetricSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("parse selector %q: %w", selector, err)
	}
	return matchers, nil
}

// PrometheusSampleSelector matches samples whose labels satisfy every matcher.
func PrometheusSampleSelector(selector string, matchers []*labels.Matcher) SampleMatcher {
	return SampleMatcher{
		match: func(sample PrometheusSample) bool {
			for _, m := range matchers {
				if !m.Matches(sample.Labels.Get(m.Name)) {
					return false
				}
			}
			return true
		},
		text: "selector " + selector,
	}
}

// LokiEntrySelector matches entries whose labels satisfy every matcher.
func LokiEntrySelector(selector string, matchers []*labels.Matcher) EntryMatcher {
	return EntryMatcher{
		match: func(entry loki.Entry) bool {
			for _, m := range matchers {
				if !m.Matches(string(entry.Labels[model.LabelName(m.Name)])) {
					return false
				}
			}
			return true
		},
		text: "selector " + selector,
	}
}
