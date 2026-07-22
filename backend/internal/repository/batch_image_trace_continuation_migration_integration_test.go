//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestBatchImageTraceContinuationMigrationSupportsRepositoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	var applied bool
	require.NoError(t, tx.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM schema_migrations
    WHERE filename = '185_batch_image_trace_continuation.sql'
)`).Scan(&applied))
	require.True(t, applied, "production migration runner must apply and record migration 185")

	migrationSQL, err := dbmigrations.FS.ReadFile("185_batch_image_trace_continuation.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err, "migration SQL must remain safe when replayed")
	requireColumn(t, tx, "batch_image_jobs", "trace_continuation", "jsonb", 0, true)

	repo := newBatchImageRepositoryWithSQL(tx)
	continuation := recording.TraceContinuation{
		TraceID:               "0123456789abcdef0123456789abcdef",
		SpanID:                "0123456789abcdef",
		TraceFlags:            1,
		TraceState:            "vendor=value",
		GenerationFingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	batchID := batchImageTestID(t, "trace-continuation")

	created, err := repo.CreateBatchImageJob(ctx, service.CreateBatchImageJobParams{
		BatchID:           batchID,
		UserID:            1001,
		Provider:          service.BatchImageProviderGeminiAPI,
		Model:             "gemini-2.5-flash-image",
		ItemCount:         1,
		TraceContinuation: &continuation,
	})
	require.NoError(t, err)
	require.Equal(t, &continuation, created.TraceContinuation)

	loaded, err := repo.GetBatchImageJobByBatchID(ctx, batchID)
	require.NoError(t, err)
	require.Equal(t, &continuation, loaded.TraceContinuation)
}
