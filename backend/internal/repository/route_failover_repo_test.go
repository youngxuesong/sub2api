package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRouteFailoverSavePreservesTargetIDs(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &routeFailoverRepository{db: db}

	policy := &service.RouteFailoverPolicy{
		SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		FailureThreshold: 3, SuccessThreshold: 1,
		Window: time.Minute, OpenCooldown: time.Minute, HalfOpenLease: 10 * time.Second,
		Targets: []service.RouteFailoverTarget{{ID: 99, TargetGroupID: 3, Enabled: true, ModelMapping: map[string]string{}}},
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)INSERT INTO route_failover_policies .*RETURNING id`).
		WithArgs(int64(1), true, 2, 3, 1, int64(60), int64(60), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(10)))
	mock.ExpectQuery(`(?s)UPDATE route_failover_targets .*WHERE id=\$1 AND policy_id=\$2.*RETURNING id`).
		WithArgs(int64(99), int64(10), int64(3), 0, true, "{}").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(99)))
	mock.ExpectExec(`DELETE FROM route_failover_targets WHERE policy_id=\$1 AND id NOT IN \(\$2\)`).
		WithArgs(int64(10), int64(99)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta(routeFailoverSnapshotQuery)).WillReturnRows(
		sqlmock.NewRows([]string{
			"policy_id", "source_group_id", "enabled", "max_attempts", "failure_threshold",
			"success_threshold", "window_seconds", "open_cooldown_seconds", "half_open_lease_seconds",
			"target_id", "target_group_id", "priority", "target_enabled", "model_mapping", "version",
		}).AddRow(10, 1, true, 2, 3, 1, 60, 60, 10, 99, 3, 0, true, []byte(`{}`), 100),
	)

	saved, err := repo.Save(context.Background(), policy)
	require.NoError(t, err)
	require.Len(t, saved.Targets, 1)
	require.Equal(t, int64(99), saved.Targets[0].ID)
	require.Equal(t, int64(3), saved.Targets[0].TargetGroupID)
	require.NoError(t, mock.ExpectationsWereMet())
}
