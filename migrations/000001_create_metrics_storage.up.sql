-- Storage of ready metrics sent by bank-statement-data (MetricsIngestService.UpsertUserMetrics,
-- grpc-api.md 7.3). Design, queries and the versioned upsert: docs/schema.md (FIN-113).
--
-- squawk-ignore-file prefer-bigint-over-int, prefer-bigint-over-smallint
-- Integers are sized by their range (Confluence «Архитектура» → «Идентификаторы»): enum numbers
-- of the contract fit smallint, full_months is int32 in the contract. The rule targets counters
-- and IDs, none of which are here.
--
-- The schema keeps only PRIMARY KEY, UNIQUE, FOREIGN KEY, NOT NULL and DEFAULT, and no triggers or
-- functions (owner's decision of 19.10.2026); value rules are checked by the service code before
-- writing — docs/schema.md, «Инварианты в коде сервиса».
--
-- Enum columns hold the numbers of the contract enums (finassist.common.v1): numbers are never
-- reused in proto, names may be. Zero (*_UNSPECIFIED) is never written.

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
    CONSTRAINT user_data_state_pkey PRIMARY KEY (user_id)
);

COMMENT ON TABLE user_data_state IS
    'Состояние данных пользователя (UserDataState) из UpsertUserMetrics, одна строка на пользователя. '
    'Новое состояние применяется, только если его calc_version не меньше сохранённого (FIN-113).';
COMMENT ON COLUMN user_data_state.user_id IS 'Идентификатор пользователя из сервиса user.';
COMMENT ON COLUMN user_data_state.calc_version IS
    'Версия расчёта запроса, который записал строку. Запрос применяется, если его calc_version не меньше этой.';
COMMENT ON COLUMN user_data_state.has_data IS 'Есть ли у пользователя хоть одна операция и хоть один покрытый выпиской день.';
COMMENT ON COLUMN user_data_state.data_from IS
    'Начало первого покрытого дня по всем загрузкам. Нет вместе с data_to, если покрытых дней нет.';
COMMENT ON COLUMN user_data_state.data_to IS
    'Начало дня после последнего покрытого дня (полуинтервал). Нет вместе с data_from, если покрытых дней нет.';
COMMENT ON COLUMN user_data_state.last_operation_at IS 'Время последней загруженной операции по всем загрузкам. Нет, если операций нет.';
COMMENT ON COLUMN user_data_state.stale_after IS
    'Момент, с которого данные устарели; считает bank-statement-data. Нет, если покрытых дней нет.';
COMMENT ON COLUMN user_data_state.full_months IS 'Число полных календарных месяцев с данными по всем загрузкам.';
COMMENT ON COLUMN user_data_state.recalculating IS 'Идёт пересчёт метрик пользователя (state.recalculating в api.md 6.1).';
COMMENT ON COLUMN user_data_state.time_zone IS
    'Часовой пояс профиля (IANA), в котором посчитаны периоды. Единственный пояс, который знает analytics.';
COMMENT ON COLUMN user_data_state.calculated_at IS 'Когда bank-statement-data посчитал метрики.';
COMMENT ON COLUMN user_data_state.applied_at IS 'Техническое: когда этот сервис последний раз применил строку.';

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
    -- finassist.common.v1.Coverage of the period: 1 complete, 2 partial (a snapshot exists only
    -- for a period with covered days); NULL in a removal marker.
    coverage      smallint,
    -- The PeriodSnapshot message as received (protobuf wire format): bounds, unread lines,
    -- calculated_at and every MetricSnapshot with its definition_version. Unknown fields and
    -- unknown metric_id survive as they are. NULL: the period was removed.
    snapshot      bytea,
    applied_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT period_snapshot_pkey PRIMARY KEY (user_id, period_type, period_key, transfer_mode)
);

COMMENT ON TABLE period_snapshot IS
    'Снимки периодов (PeriodSnapshot) из UpsertUserMetrics: одна строка на пользователя, тип периода, ключ '
    'периода и режим переводов. Снимок применяется, только если его calc_version не меньше сохранённого. '
    'Удалённый период остаётся строкой с snapshot = NULL как отметка версии, чтобы опоздавший старый снимок '
    'не вернул его (FIN-113).';
COMMENT ON COLUMN period_snapshot.user_id IS 'Идентификатор пользователя из сервиса user.';
COMMENT ON COLUMN period_snapshot.period_type IS
    'Тип периода, номер finassist.common.v1.PeriodType: 1 — месяц, 2 — квартал, 3 — год.';
COMMENT ON COLUMN period_snapshot.period_key IS
    'Ключ периода как в контракте: 2026-09, 2026-Q3, 2026. В сортировке C порядок байтов совпадает '
    'с календарным внутри типа, поэтому первичный ключ служит и навигации, и поиску последнего периода.';
COMMENT ON COLUMN period_snapshot.transfer_mode IS
    'Режим переводов, номер finassist.common.v1.TransferMode: 1 — с переводами, 2 — без переводов.';
COMMENT ON COLUMN period_snapshot.calc_version IS
    'Версия расчёта запроса, который записал строку. Снимок применяется, если его calc_version не меньше этой.';
COMMENT ON COLUMN period_snapshot.range_start IS
    'Начало периода — копия границы из снимка для SQL (ListPeriods). Нет у отметки удалённого периода.';
COMMENT ON COLUMN period_snapshot.range_end IS
    'Конец периода, не включая, — копия границы из снимка для SQL. Нет у отметки удалённого периода.';
COMMENT ON COLUMN period_snapshot.coverage IS
    'Покрытие периода, номер finassist.common.v1.Coverage: 1 — полный, 2 — частичный. Нет у отметки '
    'удалённого периода.';
COMMENT ON COLUMN period_snapshot.snapshot IS
    'Сообщение PeriodSnapshot как получено (protobuf): границы, непрочитанные строки, calculated_at и все '
    'MetricSnapshot с их definition_version. Неизвестные поля и metric_id сохраняются как есть. NULL — период удалён.';
COMMENT ON COLUMN period_snapshot.applied_at IS 'Техническое: когда этот сервис последний раз применил строку.';
