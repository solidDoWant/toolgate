-- name: Notify :exec
-- Queues a notification on the toolgate_events channel. Inside a
-- transaction it is delivered at commit, or not at all.
SELECT pg_notify('toolgate_events', @payload::text);

-- name: NotifyReply :exec
-- Queues a client's answer to a server request on the toolgate_mcp_replies
-- channel, for the replica that sent the request.
SELECT pg_notify('toolgate_mcp_replies', @payload::text);
