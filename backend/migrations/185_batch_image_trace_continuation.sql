ALTER TABLE batch_image_jobs
    ADD COLUMN IF NOT EXISTS trace_continuation JSONB;

COMMENT ON COLUMN batch_image_jobs.trace_continuation IS
    'Safe W3C trace continuation metadata for asynchronous batch image execution; excludes exporter configuration and credentials';
