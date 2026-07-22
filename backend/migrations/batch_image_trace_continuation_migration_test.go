package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration185AddsNullableBatchImageTraceContinuation(t *testing.T) {
	content, err := FS.ReadFile("185_batch_image_trace_continuation.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "ALTER TABLE batch_image_jobs")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS trace_continuation JSONB")
}
