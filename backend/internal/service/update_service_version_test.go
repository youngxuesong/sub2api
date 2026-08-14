package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompareVersionsIgnoresBuildSuffix(t *testing.T) {
	t.Parallel()

	require.Zero(t, compareVersions(
		"0.1.176-merged-0.1.176-route-failover-20260814",
		"0.1.176",
	))
	require.Negative(t, compareVersions("0.1.175-local", "0.1.176"))
	require.Positive(t, compareVersions("v0.1.177-local", "0.1.176"))
}
