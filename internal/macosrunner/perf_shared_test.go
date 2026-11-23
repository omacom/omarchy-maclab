package macosrunner

import (
	"slices"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/perf"
)

// The runner writes what labd accepts: the same reason codes, result fields
// and condition fields as internal/perf.
func TestTheRunnerSharesLabdsResultShape(t *testing.T) {
	var reasons []string
	for _, r := range Reasons {
		reasons = append(reasons, string(r))
	}
	if !slices.Equal(reasons, perf.Reasons) {
		t.Errorf("reason codes %v, labd's %v", reasons, perf.Reasons)
	}
	if !slices.Equal(ResultFields, perf.Fields) {
		t.Errorf("result fields %v, labd's %v", ResultFields, perf.Fields)
	}
	for _, f := range ConditionFields {
		if !slices.Contains(perf.ConditionFields, f) {
			t.Errorf("condition field %s is not one labd takes (%v)", f, perf.ConditionFields)
		}
	}
}
