-- 修复分组日汇总触发器的时区来源。
-- 223 迁移让触发器使用 current_setting('TimeZone')（会话时区）计算 affected_date，
-- 但 closed_before 水位与 usage_group_daily_rollups.bucket_date 均以状态行
-- timezone_name（默认 Asia/Shanghai）为准。当 DB 会话时区为 UTC 时，
-- 每天 UTC 16:00 之后（Asia/Shanghai 已跨入次日）会把刚写入的当日记录误判为
-- 已发布范围之外，错误回退水位并产生错误日桶。
-- 这里改为从 usage_group_rollup_state 读取 timezone_name，与会话时区解耦。

CREATE OR REPLACE FUNCTION invalidate_group_usage_rollup_state()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    affected_date DATE;
    published_before DATE;
    configured_timezone TEXT;
BEGIN
    SELECT timezone_name, closed_before
    INTO configured_timezone, published_before
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR UPDATE;

    IF TG_OP = 'DELETE' THEN
        affected_date := (OLD.created_at AT TIME ZONE configured_timezone)::date;
    ELSE
        IF OLD.group_id IS NULL THEN
            affected_date := (NEW.created_at AT TIME ZONE configured_timezone)::date;
        ELSIF NEW.group_id IS NULL THEN
            affected_date := (OLD.created_at AT TIME ZONE configured_timezone)::date;
        ELSE
            affected_date := LEAST(
                (OLD.created_at AT TIME ZONE configured_timezone)::date,
                (NEW.created_at AT TIME ZONE configured_timezone)::date
            );
        END IF;
    END IF;

    IF published_before > affected_date THEN
        UPDATE usage_group_rollup_state
        SET closed_before = LEAST(closed_before, affected_date),
            updated_at = NOW()
        WHERE id = 1;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION invalidate_group_usage_rollup_state_after_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    affected_date DATE;
    published_before DATE;
    configured_timezone TEXT;
BEGIN
    SELECT timezone_name, closed_before
    INTO configured_timezone, published_before
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR KEY SHARE;

    SELECT MIN((created_at AT TIME ZONE configured_timezone)::date)
    INTO affected_date
    FROM inserted_usage_logs
    WHERE group_id IS NOT NULL;

    IF affected_date IS NULL THEN
        RETURN NULL;
    END IF;

    IF published_before > affected_date THEN
        UPDATE usage_group_rollup_state
        SET closed_before = LEAST(closed_before, affected_date),
            updated_at = NOW()
        WHERE id = 1;
    END IF;

    RETURN NULL;
END;
$$;