package repo

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func countChecks(p *existenceCheckPolicy, n int, found bool) int {
	checked := 0

	for range n {
		if p.shouldCheck() {
			checked++

			p.record(found)
		}
	}

	return checked
}

func TestExistenceCheckPolicy(t *testing.T) {
	t.Parallel()

	var p existenceCheckPolicy

	// warm-up: everything is checked.
	require.Equal(t, existenceCheckWarmup, countChecks(&p, existenceCheckWarmup, false))

	// nothing is found: only one in existenceCheckProbeInterval is checked.
	require.Equal(t, 1000/existenceCheckProbeInterval, countChecks(&p, 1000, false))

	// contents start to exist: the probes find them, and within a bounded
	// number of writes every content is checked again.
	countChecks(&p, 2000, true)
	require.Equal(t, 100, countChecks(&p, 100, true))
}

func TestExistenceCheckPolicyKeepsCheckingWhenContentsExist(t *testing.T) {
	t.Parallel()

	var p existenceCheckPolicy

	// incremental uploads: one in five contents already exists.
	checked := 0

	for i := range 2000 {
		if p.shouldCheck() {
			checked++

			p.record(i%5 == 0)
		}
	}

	require.Equal(t, 2000, checked)
}
