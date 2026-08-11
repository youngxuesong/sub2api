package repository

import (
	"os"
	"strings"
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

func TestMigration221BuildsUsageSourceGroupIndexConcurrently(t *testing.T) {
	tableMigration, err := os.ReadFile("../../migrations/221_api_key_route_failover.sql")
	require.NoError(t, err)
	require.NotContains(t, string(tableMigration), "usage_logs_source_group_created_idx")

	indexMigration, err := os.ReadFile("../../migrations/221a_api_key_route_usage_source_group_index_notx.sql")
	require.NoError(t, err)
	require.Equal(t, `CREATE INDEX CONCURRENTLY IF NOT EXISTS usage_logs_source_group_created_idx
  ON usage_logs (source_group_id, created_at DESC)
  WHERE source_group_id IS NOT NULL;`, strings.TrimSpace(string(indexMigration)))
}
