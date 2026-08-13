package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSafeDateFormat(t *testing.T) {
	tests := []struct {
		name        string
		granularity string
		expected    string
	}{
		// 合法值
		{"hour", "hour", "YYYY-MM-DD HH24:00"},
		{"day", "day", "YYYY-MM-DD"},
		{"week", "week", "IYYY-IW"},
		{"month", "month", "YYYY-MM"},

		// 非法值回退到默认
		{"空字符串", "", "YYYY-MM-DD"},
		{"未知粒度 year", "year", "YYYY-MM-DD"},
		{"未知粒度 minute", "minute", "YYYY-MM-DD"},

		// 恶意字符串
		{"SQL 注入尝试", "'; DROP TABLE users; --", "YYYY-MM-DD"},
		{"带引号", "day'", "YYYY-MM-DD"},
		{"带括号", "day)", "YYYY-MM-DD"},
		{"Unicode", "日", "YYYY-MM-DD"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := safeDateFormat(tc.granularity)
			require.Equal(t, tc.expected, got, "safeDateFormat(%q)", tc.granularity)
		})
	}
}

func TestBuildUsageLogBatchInsertQuery_UsesConflictDoNothing(t *testing.T) {
	log := &service.UsageLog{
		UserID:       1,
		APIKeyID:     2,
		AccountID:    3,
		RequestID:    "req-batch-no-update",
		Model:        "gpt-5",
		InputTokens:  10,
		OutputTokens: 5,
		TotalCost:    1.2,
		ActualCost:   1.2,
		CreatedAt:    time.Now().UTC(),
	}
	prepared := prepareUsageLogInsert(log)

	query, _ := buildUsageLogBatchInsertQuery([]string{usageLogBatchKey(log.RequestID, log.APIKeyID)}, map[string]usageLogInsertPrepared{
		usageLogBatchKey(log.RequestID, log.APIKeyID): prepared,
	})

	require.Contains(t, query, "ON CONFLICT (request_id, api_key_id) DO NOTHING")
	require.NotContains(t, strings.ToUpper(query), "DO UPDATE")
}

func TestUsageLogRouteAuditPreparedInsert(t *testing.T) {
	effectiveGroupID := int64(42)
	sourceGroupID := int64(41)
	reason := "upstream_5xx"
	log := &service.UsageLog{
		UserID:              1,
		APIKeyID:            2,
		AccountID:           3,
		RequestID:           "route-audit-insert",
		Model:               "gpt-5.4",
		GroupID:             &effectiveGroupID,
		SourceGroupID:       &sourceGroupID,
		RouteFallbackUsed:   true,
		RouteAttemptCount:   2,
		RouteFallbackReason: &reason,
		RouteStickyHit:      true,
	}

	prepared := prepareUsageLogInsert(log)
	require.Len(t, prepared.args, len(usageLogInsertArgTypes))
	require.Equal(t, sql.NullInt64{Int64: effectiveGroupID, Valid: true}, prepared.args[9])
	require.Equal(t, sql.NullInt64{Int64: sourceGroupID, Valid: true}, prepared.args[10])
	require.Equal(t, true, prepared.args[11])
	require.Equal(t, 2, prepared.args[12])
	require.Equal(t, sql.NullString{String: reason, Valid: true}, prepared.args[13])
	require.Equal(t, true, prepared.args[14])

	query, _ := buildUsageLogBatchInsertQuery([]string{usageLogBatchKey(log.RequestID, log.APIKeyID)}, map[string]usageLogInsertPrepared{
		usageLogBatchKey(log.RequestID, log.APIKeyID): prepared,
	})
	require.Contains(t, compactUsageLogSQL(query), "group_id, source_group_id, route_fallback_used, route_attempt_count, route_fallback_reason, route_sticky_hit, subscription_id")
	require.Contains(t, query, "ON CONFLICT (request_id, api_key_id) DO NOTHING")
}

func TestUsageLogRouteAuditHistoricalSourceFallsBackToEffective(t *testing.T) {
	effectiveGroupID := int64(42)
	log, err := scanUsageLog(routeAuditScanStub{
		groupID:             sql.NullInt64{Int64: effectiveGroupID, Valid: true},
		sourceGroupID:       sql.NullInt64{},
		routeFallbackUsed:   false,
		routeAttemptCount:   0,
		routeFallbackReason: sql.NullString{},
		routeStickyHit:      false,
	})

	require.NoError(t, err)
	require.Equal(t, effectiveGroupID, *log.GroupID)
	require.Equal(t, effectiveGroupID, *log.SourceGroupID)
	require.Equal(t, 1, log.RouteAttemptCount)
}

func TestUsageLogRouteAuditCollectsSourceAndEffectiveGroups(t *testing.T) {
	effectiveGroupID := int64(42)
	sourceGroupID := int64(41)
	ids := collectUsageLogIDs([]service.UsageLog{{
		GroupID:       &effectiveGroupID,
		SourceGroupID: &sourceGroupID,
	}})

	require.ElementsMatch(t, []int64{sourceGroupID, effectiveGroupID}, ids.groupIDs)
}

func TestUsageLogRouteAuditHydratesSourceAndEffectiveGroupsOnAllRowQueries(t *testing.T) {
	apiKeyRepo, client := newAPIKeyRepoSQLite(t)
	queryDB, queryMock := newSQLMock(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "route-audit-hydration@test.com")
	accountID := mustCreateAPIKeyRepoAccount(t, ctx, client, "route-audit-hydration-account")

	primaryGroup, err := client.Group.Create().
		SetName("route-audit-primary").
		SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		Save(ctx)
	require.NoError(t, err)
	effectiveGroup, err := client.Group.Create().
		SetName("route-audit-effective").
		SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		Save(ctx)
	require.NoError(t, err)

	apiKey := &service.APIKey{
		UserID:  user.ID,
		Key:     "sk-route-audit-hydration",
		Name:    "route-audit-hydration",
		GroupID: &primaryGroup.ID,
		Status:  service.StatusActive,
	}
	require.NoError(t, apiKeyRepo.Create(ctx, apiKey))
	const usageID = int64(73)
	columns := strings.Split(usageLogSelectColumns, ", ")
	row := routeAuditSQLMockRow(usageID, user.ID, apiKey.ID, accountID, effectiveGroup.ID, primaryGroup.ID)
	queryMock.ExpectQuery("SELECT .* FROM usage_logs WHERE id = \\$1").
		WithArgs(usageID).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(row...))
	queryMock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM usage_logs WHERE user_id = \\$1").
		WithArgs(user.ID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	queryMock.ExpectQuery("SELECT .* FROM usage_logs WHERE user_id = \\$1 ORDER BY .* LIMIT \\$2 OFFSET \\$3").
		WithArgs(user.ID, 10, 0).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(row...))

	repo := &usageLogRepository{client: client, sql: queryDB}
	detail, err := repo.GetByID(ctx, usageID)
	require.NoError(t, err)
	require.NotNil(t, detail.Group)
	require.Equal(t, effectiveGroup.ID, detail.Group.ID)
	require.NotNil(t, detail.SourceGroup)
	require.Equal(t, primaryGroup.ID, detail.SourceGroup.ID)

	logs, _, err := repo.ListByUser(ctx, user.ID, pagination.PaginationParams{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.NotNil(t, logs[0].Group)
	require.Equal(t, effectiveGroup.ID, logs[0].Group.ID)
	require.NotNil(t, logs[0].SourceGroup)
	require.Equal(t, primaryGroup.ID, logs[0].SourceGroup.ID)
	require.NoError(t, queryMock.ExpectationsWereMet())
}

func routeAuditSQLMockRow(id, userID, apiKeyID, accountID, effectiveGroupID, sourceGroupID int64) []driver.Value {
	return []driver.Value{
		id, userID, apiKeyID, accountID, "route-audit-hydration", "gpt-5.4",
		nil, nil, nil, nil,
		effectiveGroupID, sourceGroupID, true, int64(2), "upstream_5xx", false, nil,
		int64(0), int64(0), int64(0), int64(0), int64(0), int64(0),
		int64(0), float64(0), int64(0), float64(0),
		float64(0), float64(0), float64(0), float64(0), float64(0), float64(0), float64(1), nil,
		int64(0), int64(service.RequestTypeSync), false, false,
		nil, nil, nil, nil, int64(0), nil, nil, nil, nil, nil,
		int64(0), nil, nil, nil, nil, nil, nil, false, false,
		nil, nil, nil, nil, nil, nil, time.Now().UTC(),
	}
}

func compactUsageLogSQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

type routeAuditScanStub struct {
	groupID             sql.NullInt64
	sourceGroupID       sql.NullInt64
	routeFallbackUsed   bool
	routeAttemptCount   int
	routeFallbackReason sql.NullString
	routeStickyHit      bool
}

func (s routeAuditScanStub) Scan(dest ...any) error {
	*dest[0].(*int64) = 1
	*dest[1].(*int64) = 2
	*dest[2].(*int64) = 3
	*dest[3].(*int64) = 4
	*dest[5].(*string) = "gpt-5.4"
	*dest[10].(*sql.NullInt64) = s.groupID
	*dest[11].(*sql.NullInt64) = s.sourceGroupID
	*dest[12].(*bool) = s.routeFallbackUsed
	*dest[13].(*int) = s.routeAttemptCount
	*dest[14].(*sql.NullString) = s.routeFallbackReason
	*dest[15].(*bool) = s.routeStickyHit
	*dest[len(dest)-1].(*time.Time) = time.Now().UTC()
	return nil
}
