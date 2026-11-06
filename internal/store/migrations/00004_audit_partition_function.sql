-- +goose Up

-- toolgate_ensure_audit_partitions creates the audit_events partitions for
-- the month of now() and the next one, if they don't exist. It runs with the
-- rights of its owner, the role that runs the migrations and owns
-- audit_events, so a gateway with data rights only can call it. Calls are
-- serialized by the advisory lock store.partitionLockKey
-- (0x746f6f6c67617465). Partition names are built from dates alone.
-- +goose StatementBegin
CREATE FUNCTION toolgate_ensure_audit_partitions() RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
AS $$
DECLARE
    month     timestamptz := date_trunc('month', now());
    month_end timestamptz;
    part_name text;
BEGIN
    PERFORM pg_advisory_xact_lock(8390047142196114533);

    FOR i IN 1..2 LOOP
        month_end := month + interval '1 month';
        part_name := 'audit_events_' || to_char(month, 'YYYY_MM');

        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I.%I PARTITION OF %I.audit_events FOR VALUES FROM (%L) TO (%L)',
            current_schema(), part_name, current_schema(), month, month_end
        );

        month := month_end;
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- The schema is known only at run time. pg_temp goes last so no temporary
-- object can shadow the function's tables. UTC matches the month bounds of
-- the partitions created before this function existed.
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.toolgate_ensure_audit_partitions() SET search_path = %I, pg_temp',
        current_schema(), current_schema()
    );
    EXECUTE format(
        'ALTER FUNCTION %I.toolgate_ensure_audit_partitions() SET TimeZone = %L',
        current_schema(), 'UTC'
    );
END;
$$;
-- +goose StatementEnd

REVOKE EXECUTE ON FUNCTION toolgate_ensure_audit_partitions() FROM PUBLIC;
