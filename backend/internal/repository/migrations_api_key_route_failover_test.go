package repository

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration221DoesNotCopyLegacyPolicies(t *testing.T) {
	body, err := os.ReadFile("../../migrations/221_api_key_route_failover.sql")
	require.NoError(t, err)
	sqlText := string(body)
	require.NotContains(t, sqlText, "FROM route_failover_policies")
	require.NotContains(t, sqlText, "FROM route_failover_targets")
}
