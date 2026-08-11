package schema_test

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/ent/schema"
	"github.com/stretchr/testify/require"

	"entgo.io/ent"
)

func fieldNames(fields []ent.Field) []string {
	names := make([]string, 0, len(fields))
	for _, item := range fields {
		names = append(names, item.Descriptor().Name)
	}
	return names
}

func TestAPIKeyRouteFailoverSchemaFields(t *testing.T) {
	apiKeyFields := fieldNames((schema.APIKey{}).Fields())
	require.Contains(t, apiKeyFields, "route_config_version")
	require.Contains(t, apiKeyFields, "failover_risk_acknowledged_at")

	usageFields := fieldNames((schema.UsageLog{}).Fields())
	for _, name := range []string{"source_group_id", "route_fallback_used", "route_attempt_count", "route_fallback_reason", "route_sticky_hit"} {
		require.Contains(t, usageFields, name)
	}

	targetFields := fieldNames((schema.APIKeyRouteFailoverTarget{}).Fields())
	require.Equal(t, []string{"api_key_id", "target_group_id", "priority"}, targetFields)
}
