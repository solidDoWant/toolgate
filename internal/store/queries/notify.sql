-- name: Notify :exec
-- Queues a notification on the toolgate_events channel. Inside a
-- transaction it is delivered at commit, or not at all.
SELECT pg_notify('toolgate_events', @payload::text);
