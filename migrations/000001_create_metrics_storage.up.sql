-- Storage of ready metrics sent by bank-statement-data (MetricsIngestService.UpsertUserMetrics,
-- grpc-api.md 7.3). Design, queries and the versioned upsert: docs/schema.md (FIN-113).
--
-- squawk-ignore-file prefer-bigint-over-int, prefer-bigint-over-smallint
-- Integers are sized by their range (Confluence «Архитектура» → «Идентификаторы»): enum numbers
-- of the contract fit smallint, full_months is int32 in the contract. The rule targets counters
-- and IDs, none of which are here.

-- Enum columns hold the numbers of the contract enums (finassist.common.v1): numbers are never
-- reused in proto, names may be. Zero (*_UNSPECIFIED) is never stored.

-- State of the user's data (UserDataState): one row per user, versioned on its own.
CREATE TABLE user_data_state (
    user_id           uuid        NOT NULL,
    -- calc_version of the request that wrote this row; a request applies only when its
    -- calc_version >= this one.
    calc_version      bigint      NOT NULL,
    has_data          boolean     NOT NULL,
    -- Covered days over all uploads, half-open: start of the first covered day and start of
    -- the day after the last one. Absent together: no covered days.
    data_from         timestamptz,
    data_to           timestamptz,
    last_operation_at timestamptz,
    -- Moment from which the data is stale (computed by bank-statement-data); absent without
    -- covered days.
    stale_after       timestamptz,
    full_months       integer     NOT NULL,
    recalculating     boolean     NOT NULL,
    -- IANA zone of the profile the periods were computed in; the only time zone analytics knows.
    time_zone         text        NOT NULL,
    calculated_at     timestamptz NOT NULL,
    -- Technical: when this service last applied the row.
    applied_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_data_state_pkey PRIMARY KEY (user_id),
    CONSTRAINT user_data_state_calc_version_check CHECK (calc_version > 0),
    CONSTRAINT user_data_state_bounds_check CHECK (
        (data_from IS NULL) = (data_to IS NULL) AND data_from < data_to
    ),
    -- stale_after is data_to + 14 calendar days in the profile zone, so always after data_to.
    CONSTRAINT user_data_state_stale_after_check CHECK (
        (stale_after IS NULL) = (data_to IS NULL) AND stale_after > data_to
    ),
    CONSTRAINT user_data_state_has_data_check CHECK (
        NOT has_data OR (data_from IS NOT NULL AND last_operation_at IS NOT NULL)
    ),
    CONSTRAINT user_data_state_full_months_check CHECK (full_months >= 0),
    CONSTRAINT user_data_state_time_zone_check CHECK (time_zone <> '' AND length(time_zone) <= 64)
);

COMMENT ON TABLE user_data_state IS
    'UserDataState from UpsertUserMetrics, one row per user, applied when calc_version >= stored (FIN-113)';

-- Snapshots of periods (PeriodSnapshot): one row per (user, period type, period key, transfer mode).
-- A removed period (removed_periods) keeps its rows with snapshot = NULL as a version marker,
-- so that an older snapshot delivered late does not bring the period back.
CREATE TABLE period_snapshot (
    user_id       uuid        NOT NULL,
    -- finassist.common.v1.PeriodType: 1 month, 2 quarter, 3 year.
    period_type   smallint    NOT NULL,
    -- Calendar key as in the contract: 2026-09, 2026-Q3, 2026. In the C collation byte order is
    -- calendar order within a type, so the primary key also serves navigation and the latest period.
    period_key    text        COLLATE "C" NOT NULL,
    -- finassist.common.v1.TransferMode: 1 with transfers, 2 without.
    transfer_mode smallint    NOT NULL,
    calc_version  bigint      NOT NULL,
    -- Projection of the snapshot for SQL (ListPeriods); NULL in a removal marker.
    range_start   timestamptz,
    range_end     timestamptz,
    -- finassist.common.v1.Coverage of the period: 1 complete, 2 partial. A snapshot exists
    -- only for a period with covered days, so 3 (no data) is never stored.
    coverage      smallint,
    -- The PeriodSnapshot message as received (protobuf wire format): bounds, unread lines,
    -- calculated_at and every MetricSnapshot with its definition_version. Unknown fields and
    -- unknown metric_id survive as they are. NULL: the period was removed.
    snapshot      bytea,
    applied_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT period_snapshot_pkey PRIMARY KEY (user_id, period_type, period_key, transfer_mode),
    CONSTRAINT period_snapshot_calc_version_check CHECK (calc_version > 0),
    CONSTRAINT period_snapshot_period_type_check CHECK (period_type IN (1, 2, 3)),
    CONSTRAINT period_snapshot_transfer_mode_check CHECK (transfer_mode IN (1, 2)),
    CONSTRAINT period_snapshot_period_key_check CHECK (
        CASE period_type
            WHEN 1 THEN period_key ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'
            WHEN 2 THEN period_key ~ '^[0-9]{4}-Q[1-4]$'
            WHEN 3 THEN period_key ~ '^[0-9]{4}$'
        END
    ),
    CONSTRAINT period_snapshot_removed_check CHECK (
        CASE WHEN snapshot IS NULL
            THEN range_start IS NULL AND range_end IS NULL AND coverage IS NULL
            ELSE range_start IS NOT NULL AND range_end IS NOT NULL AND coverage IS NOT NULL
        END
    ),
    CONSTRAINT period_snapshot_range_check CHECK (range_start < range_end),
    CONSTRAINT period_snapshot_coverage_check CHECK (coverage IN (1, 2))
);

COMMENT ON TABLE period_snapshot IS
    'PeriodSnapshot from UpsertUserMetrics per (user, period, key, transfer mode), applied when calc_version >= stored; snapshot NULL = removed period kept as a version marker (FIN-113)';
