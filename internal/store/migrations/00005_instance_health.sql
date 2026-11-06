-- +goose Up

-- health_check is the template's custom health check (JSON, with http.service
-- rendered) as it was when the instance started, or null without one.
ALTER TABLE instances ADD COLUMN health_check JSONB;

-- unhealthy_since is when the custom check or an object marked the instance
-- unhealthy; null while it is healthy. unhealthy_reason says why.
ALTER TABLE instances ADD COLUMN unhealthy_since TIMESTAMPTZ;
ALTER TABLE instances ADD COLUMN unhealthy_reason TEXT NOT NULL DEFAULT '';

-- repairs lists the times of the repairs made for the template and scope key,
-- carried to each replacement row.
ALTER TABLE instances ADD COLUMN repairs TIMESTAMPTZ[] NOT NULL DEFAULT '{}';
