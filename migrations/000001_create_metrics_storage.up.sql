-- Storage of ready metrics sent by bank-statement-data (MetricsIngestService.UpsertUserMetrics,
-- grpc-api.md 7.3). Design, queries and the versioned upsert: docs/schema.md (FIN-113).
-- Metrics are stored normalised (owner's decision of 20.10.2026): a header per period snapshot,
-- its gaps and a row per metric; only the details oneof of a metric is jsonb.
--
-- squawk-ignore-file prefer-bigint-over-int, prefer-bigint-over-smallint
-- Integers are sized by their range (Confluence «Архитектура» → «Идентификаторы»): enum numbers
-- of the contract and the ordinal of a gap fit smallint; full_months, unread_lines_count,
-- definition_version and the month counts of a lock are int32 in the contract; calc_version is
-- integer as in bank-statement-data, which issues it (int64 in the contract: a larger one is
-- refused by the service, owner's decision in the review of FIN-113). The rule targets
-- counters and IDs, none of which are here; money is bigint (*_minor).
--
-- The schema keeps only PRIMARY KEY, UNIQUE, FOREIGN KEY, NOT NULL and DEFAULT, and no triggers or
-- functions (owner's decision of 19.10.2026); value rules are checked by the service code before
-- writing — docs/schema.md, «Инварианты в коде сервиса».
--
-- Enum columns hold the numbers of the contract enums (finassist.common.v1, finassist.analytics.v1):
-- numbers are never reused in proto, names may be. Zero (*_UNSPECIFIED) is never written: an
-- optional enum of the contract that is unset is NULL.

-- State of the user's data (UserDataState): one row per user, versioned on its own.
CREATE TABLE user_data_state (
    user_id           uuid        NOT NULL,
    -- calc_version of the request that wrote this row; a request applies only when its
    -- calc_version >= this one.
    calc_version      integer     NOT NULL,
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
COMMENT ON COLUMN user_data_state.has_data IS
    'Есть ли у пользователя хоть одна операция и хоть один покрытый день — день внутри периода выписки '
    '(DTSTART..DTEND) какой-либо загрузки, а не день с операциями (calc.md 4).';
COMMENT ON COLUMN user_data_state.data_from IS
    'Начало (00:00 в поясе профиля) первого покрытого дня по всем загрузкам; покрытый день — день внутри '
    'периода выписки (DTSTART..DTEND), а не день с операциями (calc.md 4). Нет вместе с data_to, если покрытых '
    'дней нет.';
COMMENT ON COLUMN user_data_state.data_to IS
    'Начало дня, следующего за последним покрытым днём по всем загрузкам (конец не включая); покрытый день — '
    'день внутри периода выписки (DTSTART..DTEND). Нет вместе с data_from, если покрытых дней нет.';
COMMENT ON COLUMN user_data_state.last_operation_at IS 'Время последней загруженной операции по всем загрузкам. Нет, если операций нет.';
COMMENT ON COLUMN user_data_state.stale_after IS
    'Момент, с которого данные устарели; считает bank-statement-data. Нет, если покрытых дней нет.';
COMMENT ON COLUMN user_data_state.full_months IS 'Число полных календарных месяцев с данными по всем загрузкам.';
COMMENT ON COLUMN user_data_state.recalculating IS 'Идёт пересчёт метрик пользователя (state.recalculating в api.md 6.1).';
COMMENT ON COLUMN user_data_state.time_zone IS
    'Часовой пояс профиля (IANA), в котором посчитаны периоды. Единственный пояс, который знает analytics.';
COMMENT ON COLUMN user_data_state.calculated_at IS 'Когда bank-statement-data посчитал метрики.';
COMMENT ON COLUMN user_data_state.applied_at IS 'Техническое: когда этот сервис последний раз применил строку.';

-- Headers of period snapshots (PeriodSnapshot without its metrics): one row per
-- (user, period type, period key, transfer mode). A removed period (removed_periods) keeps its
-- row with removed = true and empty data columns as a version marker, so that an older snapshot
-- delivered late does not bring the period back. The row is also the lock that serialises the
-- writers of the period's gaps and metrics.
CREATE TABLE period_snapshot (
    user_id            uuid        NOT NULL,
    -- finassist.common.v1.PeriodType: 1 month, 2 quarter, 3 year.
    period_type        smallint    NOT NULL,
    -- Calendar key as in the contract: 2026-09, 2026-Q3, 2026. In the C collation byte order is
    -- calendar order within a type, so the primary key also serves navigation and the latest period.
    period_key         text        COLLATE "C" NOT NULL,
    -- finassist.common.v1.TransferMode: 1 with transfers, 2 without.
    transfer_mode      smallint    NOT NULL,
    calc_version       integer     NOT NULL,
    -- true: a removal marker; the data columns below are NULL.
    removed            boolean     NOT NULL,
    -- PeriodSnapshot.range.
    range_start        timestamptz,
    range_end          timestamptz,
    -- PeriodSnapshot.bounds: coverage (finassist.common.v1.Coverage, 1 complete, 2 partial),
    -- data_from, data_to; the gaps are in period_gap.
    coverage           smallint,
    data_from          timestamptz,
    data_to            timestamptz,
    unread_lines_count integer,
    calculated_at      timestamptz,
    applied_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT period_snapshot_pkey PRIMARY KEY (user_id, period_type, period_key, transfer_mode)
);

COMMENT ON TABLE period_snapshot IS
    'Заголовки снимков периодов (PeriodSnapshot без метрик) из UpsertUserMetrics: одна строка на пользователя, '
    'тип периода, ключ периода и режим переводов. Снимок применяется, только если его calc_version не меньше '
    'сохранённого. Удалённый период остаётся строкой с removed = true и пустыми полями данных как отметка '
    'версии, чтобы опоздавший старый снимок не вернул его. Строка — блокировка, под которой пишутся пропуски '
    'и метрики периода (FIN-113).';
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
COMMENT ON COLUMN period_snapshot.removed IS
    'Отметка удалённого периода (removed_periods): true — данных за период нет, поля данных пусты, '
    'пропусков и метрик нет; строка хранит только версию.';
COMMENT ON COLUMN period_snapshot.range_start IS
    'Календарное начало самого периода в поясе профиля (PeriodSnapshot.range.start): для месяца 2026-09 — '
    '1 сентября 2026, 00:00. Не зависит от выписок. Нет у отметки удалённого периода.';
COMMENT ON COLUMN period_snapshot.range_end IS
    'Календарный конец самого периода в поясе профиля, не включая (PeriodSnapshot.range.end): для месяца 2026-09 — '
    '1 октября 2026, 00:00. Не зависит от выписок. Нет у отметки удалённого периода.';
COMMENT ON COLUMN period_snapshot.coverage IS
    'Покрыт ли период выписками полностью или частично (bounds.coverage) — по периодам выписок (DTSTART..DTEND), '
    'а не по дням с операциями; номер finassist.common.v1.Coverage: 1 — полностью (период закончился и покрыт '
    'каждый день), 2 — частично. Нет у отметки удалённого периода.';
COMMENT ON COLUMN period_snapshot.data_from IS
    'С какого момента период покрыт выписками (bounds.data_from): начало (00:00) первого покрытого дня в периоде; '
    'покрытый день — день внутри периода выписки (DTSTART..DTEND), а не день с операциями (calc.md 4). При полном '
    'покрытии совпадает с range_start. Нет — покрытых дней нет или это отметка удаления.';
COMMENT ON COLUMN period_snapshot.data_to IS
    'До какого момента период покрыт выписками (bounds.data_to): время последней операции, если она в последний '
    'покрытый день периода, иначе конец этого дня (23:59:59). При полном покрытии это последний день периода. '
    'Нет — покрытых дней нет или это отметка удаления.';
COMMENT ON COLUMN period_snapshot.unread_lines_count IS
    'Незакрытых непрочитанных строк выписок за период (FIN-79). Нет у отметки удалённого периода.';
COMMENT ON COLUMN period_snapshot.calculated_at IS 'Когда посчитан снимок. Нет у отметки удалённого периода.';
COMMENT ON COLUMN period_snapshot.applied_at IS 'Техническое: когда этот сервис последний раз применил строку.';

-- Gaps of a snapshot (PeriodSnapshot.bounds.gaps) in their order.
CREATE TABLE period_gap (
    user_id       uuid        NOT NULL,
    period_type   smallint    NOT NULL,
    period_key    text        COLLATE "C" NOT NULL,
    transfer_mode smallint    NOT NULL,
    -- Position in bounds.gaps, from 0.
    ordinal       smallint    NOT NULL,
    gap_start     timestamptz NOT NULL,
    gap_end       timestamptz NOT NULL,
    CONSTRAINT period_gap_pkey PRIMARY KEY (user_id, period_type, period_key, transfer_mode, ordinal),
    CONSTRAINT period_gap_period_snapshot_fkey FOREIGN KEY (user_id, period_type, period_key, transfer_mode)
        REFERENCES period_snapshot (user_id, period_type, period_key, transfer_mode) ON DELETE CASCADE
);

COMMENT ON TABLE period_gap IS
    'Пропуски в данных периода (PeriodSnapshot.bounds.gaps): дни внутри данных, которых нет ни в одной загрузке. '
    'Строка на пропуск, по порядку; набор заменяется целиком вместе со снимком (FIN-113).';
COMMENT ON COLUMN period_gap.user_id IS 'Идентификатор пользователя, часть ключа снимка.';
COMMENT ON COLUMN period_gap.period_type IS 'Тип периода снимка, номер finassist.common.v1.PeriodType.';
COMMENT ON COLUMN period_gap.period_key IS 'Ключ периода снимка, как в period_snapshot.';
COMMENT ON COLUMN period_gap.transfer_mode IS 'Режим переводов снимка, номер finassist.common.v1.TransferMode.';
COMMENT ON COLUMN period_gap.ordinal IS 'Порядковый номер пропуска в bounds.gaps, с нуля.';
COMMENT ON COLUMN period_gap.gap_start IS 'Начало пропуска, включительно.';
COMMENT ON COLUMN period_gap.gap_end IS 'Конец пропуска, не включая.';

-- Metrics of a snapshot (MetricSnapshot): one row per metric_id, an unknown metric_id included.
-- Common fields of Metric are columns; the details oneof is protojson. A group of nullable
-- columns stands for one optional message of the contract and is NULL together when it is absent.
CREATE TABLE period_metric (
    user_id               uuid        NOT NULL,
    period_type           smallint    NOT NULL,
    period_key            text        COLLATE "C" NOT NULL,
    transfer_mode         smallint    NOT NULL,
    -- Registry ID (tiles.expense, …); C collation: the order of the key is byte order.
    metric_id             text        COLLATE "C" NOT NULL,
    definition_version    integer     NOT NULL,
    -- finassist.analytics.v1.MetricStatus.
    status                smallint    NOT NULL,
    -- Metric.value_minor: NULL is absent, not zero.
    value_minor           bigint,
    -- Metric.range: the range of a comparison.
    range_start           timestamptz,
    range_end             timestamptz,
    -- Metric.lock without its text (analytics builds the text): present when lock_reason is set.
    lock_reason           smallint,
    lock_required         integer,
    lock_available        integer,
    lock_available_from   timestamptz,
    -- Metric.basis: present when basis_full_months is set.
    basis_start           timestamptz,
    basis_end             timestamptz,
    basis_full_months     integer,
    -- Metric.filters (OperationsFilter): present when filter_from is set (an analytics filter
    -- always has the period). q, amount_from_minor, amount_to_minor are never set by analytics
    -- (calc.md 5, «Выборки») and have no columns.
    filter_from           timestamptz,
    filter_to             timestamptz,
    filter_category_id    text,
    filter_kind           smallint,
    filter_transfer_mode  smallint,
    filter_selection      text,
    filter_selection_name text,
    -- The details oneof as protojson of a Metric that has only it: {"categories": {…}}.
    details               jsonb,
    CONSTRAINT period_metric_pkey PRIMARY KEY (user_id, period_type, period_key, transfer_mode, metric_id),
    CONSTRAINT period_metric_period_snapshot_fkey FOREIGN KEY (user_id, period_type, period_key, transfer_mode)
        REFERENCES period_snapshot (user_id, period_type, period_key, transfer_mode) ON DELETE CASCADE
);

COMMENT ON TABLE period_metric IS
    'Метрики снимка периода (MetricSnapshot): строка на metric_id, неизвестный metric_id сохраняется. Общие поля '
    'Metric — столбцами, вариант details — protojson. Набор метрик периода заменяется целиком вместе со снимком: '
    'метрик, которых нет в новом снимке, не остаётся (FIN-113).';
COMMENT ON COLUMN period_metric.user_id IS 'Идентификатор пользователя, часть ключа снимка.';
COMMENT ON COLUMN period_metric.period_type IS 'Тип периода снимка, номер finassist.common.v1.PeriodType.';
COMMENT ON COLUMN period_metric.period_key IS 'Ключ периода снимка, как в period_snapshot.';
COMMENT ON COLUMN period_metric.transfer_mode IS 'Режим переводов снимка, номер finassist.common.v1.TransferMode.';
COMMENT ON COLUMN period_metric.metric_id IS
    'Идентификатор метрики из реестра bank-statement-data (tiles.expense, insights.bank_fees, …); неизвестный сохраняется, '
    'но не отображается.';
COMMENT ON COLUMN period_metric.definition_version IS
    'Версия определения метрики в реестре (FIN-6); хранится рядом со значением, при чтении ни с чем не сравнивается.';
COMMENT ON COLUMN period_metric.status IS
    'Состояние метрики, номер finassist.analytics.v1.MetricStatus: 1 — готова, 2 — предварительна, 3 — закрыта, '
    '4 — ничего не найдено.';
COMMENT ON COLUMN period_metric.value_minor IS
    'Значение метрики в копейках со знаком (Metric.value_minor). Нет — значения нет (это не ноль).';
COMMENT ON COLUMN period_metric.range_start IS
    'Начало отрезка, за который взято значение сравнения (Metric.range). Нет — у метрики нет отрезка.';
COMMENT ON COLUMN period_metric.range_end IS 'Конец отрезка сравнения, не включая (Metric.range). Нет вместе с range_start.';
COMMENT ON COLUMN period_metric.lock_reason IS
    'Причина блокировки, номер finassist.analytics.v1.LockReason (Metric.lock.reason). Нет — блокировки нет, '
    'и остальные lock_* пусты. Текст блокировки не хранится: его строит analytics.';
COMMENT ON COLUMN period_metric.lock_required IS 'Сколько месяцев нужно, чтобы метрика открылась (lock.required). Нет — не задано.';
COMMENT ON COLUMN period_metric.lock_available IS 'Сколько месяцев есть сейчас (lock.available). Нет — не задано.';
COMMENT ON COLUMN period_metric.lock_available_from IS
    'Момент, с которого метрика откроется (lock.available_from). Нет — не задан.';
COMMENT ON COLUMN period_metric.basis_start IS 'Начало базы расчёта (Metric.basis.range). Нет — базы нет.';
COMMENT ON COLUMN period_metric.basis_end IS 'Конец базы расчёта, не включая (Metric.basis.range). Нет — базы нет.';
COMMENT ON COLUMN period_metric.basis_full_months IS
    'Сколько полных месяцев вошло в базу (Metric.basis.full_months). Нет — базы нет, и basis_* пусты.';
COMMENT ON COLUMN period_metric.filter_from IS
    'Начало периода перехода к операциям, включительно (Metric.filters.from). Нет — перехода нет, и filter_* пусты.';
COMMENT ON COLUMN period_metric.filter_to IS 'Конец периода перехода к операциям, не включая (filters.to).';
COMMENT ON COLUMN period_metric.filter_category_id IS 'Категория перехода к операциям (filters.category_id). Нет — без категории.';
COMMENT ON COLUMN period_metric.filter_kind IS
    'Направление перехода, номер finassist.common.v1.KindFilter (filters.kind): 1 — расходы, 2 — доходы. Нет — любое.';
COMMENT ON COLUMN period_metric.filter_transfer_mode IS
    'Выборка по правилам аналитики, номер finassist.common.v1.TransferMode (filters.transfer_mode). Нет — не задана.';
COMMENT ON COLUMN period_metric.filter_selection IS
    'Непрозрачный токен выборки bank-statement-data (filters.selection). Нет — выборки нет.';
COMMENT ON COLUMN period_metric.filter_selection_name IS
    'Название выборки для показа (filters.selection_name). Нет — названия нет.';
COMMENT ON COLUMN period_metric.details IS
    'Типизированные детали метрики (вариант oneof details) в protojson сообщения Metric, где заполнены только '
    'они: ключ — имя варианта, например {"categories": {…}}. Нет — у метрики нет деталей.';
