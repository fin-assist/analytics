# Схема БД `analytics`

Задача — [FIN-113](https://finassist.youtrack.cloud/issue/FIN-113). Миграция — `migrations/000001_create_metrics_storage.up.sql`; запросы ниже проверяются тестами `migrations/schema_test.go` на PostgreSQL 18. Смысл полей — `fin-assist/contracts`: `docs/grpc-api.md` 7.3, `proto/finassist/analytics/v1/analytics.proto`, `docs/api.md` 6.1, 6.2.

`analytics` хранит то, что присылает `bank-statement-data` в `UpsertUserMetrics`, и ничего не считает сам. Своего состояния о пользователе нет: пояс и границы данных — только из `UserDataState`.

## Таблицы

| Таблица | Строка | Ключ |
|---|---|---|
| `user_data_state` | `UserDataState` пользователя | `user_id` |
| `period_snapshot` | `PeriodSnapshot` одного периода в одном режиме переводов, либо метка удалённого периода | `(user_id, period_type, period_key, transfer_mode)` |
| `tombstone_*` | библиотека `go-tombstone` (FIN-110), своим источником миграций | — |

Outbox и идемпотентности нет: сервис только получает события, повтор безопасен по версии.

### `period_snapshot`

- **Снимок целиком — `bytea` с сообщением `PeriodSnapshot` в формате protobuf**, как он пришёл: границы и полнота, `unread_lines_count`, `calculated_at`, все `MetricSnapshot` с `metric_id` и `definition_version`. Новая метрика, новое поле метрики, новый вид `details` — без миграции. Неизвестный `metric_id` и неизвестные поля сохраняются как есть (контракт: «сохраняется, но не отображается») и появятся после выкатки композиции.
- **Проекция для SQL** — `range_start`, `range_end`, `coverage`: только то, что нужно `ListPeriods` без разбора protobuf. Источник правды — `snapshot`; проекцию пишет тот же оператор.
- **Метка удалённого периода** — строка с `snapshot IS NULL` и версией запроса с `removed_periods` (по строке на оба режима). Она нужна, чтобы снимок более старой версии, доставленный после удаления, не вернул период: версия сравнивается «для этого ключа», и после `DELETE` сравнивать было бы не с чем. Меток не больше, чем периодов у пользователя за всё время (сотни строк за 10 лет).
- `period_type`, `transfer_mode`, `coverage` — номера значений proto-enum (`finassist.common.v1`), не имена: номера в proto не переиспользуются. `*_UNSPECIFIED` не хранится (CHECK).
- `period_key` — строка контракта (`2026-09`, `2026-Q3`, `2026`), CHECK формата по типу. Колонка в `COLLATE "C"`: побайтовый порядок совпадает с календарным внутри типа, поэтому первичный ключ обслуживает и навигацию, и «последний период».

### `user_data_state`

Колонками, а не protobuf: полей девять, набор растёт медленно, а инварианты (границы парами, `stale_after` вместе с границами и после `data_to`, `has_data` ⇒ есть операция) проверяются ограничениями. Новое поле — новая nullable-колонка, миграция без блокировок.

## Инварианты в схеме

| Ограничение | Что не даёт записать |
|---|---|
| `*_calc_version_check` | версию ≤ 0 (у `bank-statement-data` — sequence с 1) |
| `user_data_state_bounds_check` | одну границу без другой, пустой интервал |
| `user_data_state_stale_after_check` | `stale_after` без границ или не позже `data_to` |
| `user_data_state_has_data_check` | `has_data` без покрытых дней или без последней операции |
| `user_data_state_time_zone_check` | пустой или длиннее 64 символов пояс |
| `period_snapshot_period_type_check`, `_transfer_mode_check` | `UNSPECIFIED` и неизвестные значения |
| `period_snapshot_period_key_check` | ключ не того формата для типа (`2026-Q3` у месяца, `2026-13`) |
| `period_snapshot_removed_check` | снимок без проекции или метку с проекцией |
| `period_snapshot_range_check` | пустой интервал периода |
| `period_snapshot_coverage_check` | `no_data` — снимок есть только у периода с покрытыми днями |

Чего схема не проверяет и что остаётся обработчику: что `period_key` в колонке совпадает с ключом внутри `snapshot`; что ключ встречается в запросе один раз (иначе оператор падает целиком с `cardinality_violation`, тест это показывает); соответствие версий определений реестру.

## Запись: `UpsertUserMetrics`

Одна транзакция `READ COMMITTED`: сначала состояние, затем все снимки и метки одним оператором.

```sql
-- user_data_state: применяется, если версия не меньше сохранённой
INSERT INTO user_data_state AS s (user_id, calc_version, has_data, data_from, data_to, last_operation_at,
                                  stale_after, full_months, recalculating, time_zone, calculated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (user_id) DO UPDATE
SET calc_version = EXCLUDED.calc_version, has_data = EXCLUDED.has_data, /* … остальные поля … */
    applied_at = now()
WHERE s.calc_version <= EXCLUDED.calc_version
RETURNING s.user_id;

-- period_snapshot: снимки и метки removed_periods (snapshot = NULL) одного запроса
INSERT INTO period_snapshot AS s
    (user_id, period_type, period_key, transfer_mode, calc_version, range_start, range_end, coverage, snapshot)
SELECT $1, p.period_type, p.period_key, p.transfer_mode, $2, p.range_start, p.range_end, p.coverage, p.snapshot
FROM unnest($3::smallint[], $4::text[], $5::smallint[], $6::timestamptz[], $7::timestamptz[], $8::smallint[], $9::bytea[])
    AS p (period_type, period_key, transfer_mode, range_start, range_end, coverage, snapshot)
ORDER BY p.period_type, p.period_key COLLATE "C", p.transfer_mode
ON CONFLICT (user_id, period_type, period_key, transfer_mode) DO UPDATE
SET calc_version = EXCLUDED.calc_version, range_start = EXCLUDED.range_start, range_end = EXCLUDED.range_end,
    coverage = EXCLUDED.coverage, snapshot = EXCLUDED.snapshot, applied_at = now()
WHERE s.calc_version <= EXCLUDED.calc_version
RETURNING s.period_type, s.period_key, s.transfer_mode, s.snapshot IS NULL AS removed;
```

- **Конкурентная доставка.** При конфликте `ON CONFLICT DO UPDATE` блокирует существующую строку и проверяет `WHERE` по последней зафиксированной версии, даже если условие ложно. Два запроса на один ключ сериализуются на строке, побеждает большая версия; первая вставка нового ключа сериализуется на уникальном индексе. Порядок доставки не важен.
- **`ORDER BY` по первичному ключу обязателен.** Запросы одного пользователя пересекаются по ключам; без общего порядка они блокируют строки встречно, и тест конкурентной доставки ловит `deadlock detected` (проверено удалением `ORDER BY`). По той же причине состояние пишется первым во всех запросах.
- **`<=`, а не `<`**: повтор той же версии (outbox at-least-once) применяется и ничего не меняет — `calc_version` однозначно определяет расчёт. Версия сравнивается для каждого ключа отдельно, а не для пользователя: снимок версии 4 по периоду, которого в версии 5 не было, применяется.
- **Ответ.** `applied_periods` — число строк `RETURNING` со снимком; `skipped_stale_periods` — число снимков запроса минус применённые. Метки удаления в счётчики не входят (в контракте счётчики — про снимки).
- **Атомарность снимка** — одна строка; частично записанного снимка не бывает. Большой пересчёт приходит несколькими запросами (≤ 4 МиБ), каждый — своей транзакцией.

## Чтение

Все запросы — по префиксу первичного ключа; отдельных индексов нет. Тест `TestQueriesUsePrimaryKey` проверяет, что у каждого запроса есть план по первичному ключу без сортировки.

| Запрос | SQL (полностью — в `schema_test.go`) |
|---|---|
| Последний период с данными (6.1 без `date`) | `SELECT max(period_key) … WHERE user_id = $1 AND period_type = $2 AND snapshot IS NOT NULL` |
| 6.1 за период: состояние, снимок и соседи **одним оператором** | `user_data_state` `LEFT JOIN period_snapshot` по полному ключу плюс два подзапроса `max(period_key) … < $3` и `min(period_key) … > $3` |
| 6.2 список периодов типа с полнотой | `SELECT DISTINCT ON (period_key) period_key, range_start, range_end, coverage … ORDER BY period_key DESC, transfer_mode DESC` |
| `PurgeUserData` | `WITH periods AS (DELETE FROM period_snapshot WHERE user_id = $1) DELETE FROM user_data_state WHERE user_id = $1` |

- **Один оператор для 6.1** — не только ради одного обращения: при чтении с реплики состояние, снимок и навигация видны в одном снимке данных, и между ними не встанет запрос записи. Нет строки состояния — `has_data = false`; нет снимка по ключу и режиму — все метрики периода `UNAVAILABLE`.
- Метки удаления (`snapshot IS NULL`) исключаются из навигации, списка периодов и «последнего периода» (тесты).
- Точный ключ периода из неточного `period_key` (`quarter` + `2026-09` → `2026-Q3`) и `is_current`/`stale` считаются в коде из `time_zone` состояния и текущего момента — схеме они не нужны.
- `ListPeriods` берёт одну строку на период из двух режимов: `coverage` и границы от режима переводов не зависят.

## `unavailable` и версии определений (FIN-6)

`definition_version` хранится внутри снимка рядом со значением каждой метрики. Метрика отдаётся как `UNAVAILABLE`, если её нет в снимке или её версия ниже требуемой. Откуда `analytics` берёт требуемую версию, контракт не говорит — это открытый вопрос (ниже). Предлагаемый вариант не требует таблицы: минимальная версия на `metric_id` — в композиции экрана (FIN-5), выкатывается вместе с ней. Если требуемые версии будут приходить от `bank-statement-data` данными, понадобится таблица `metric_definition (metric_id, definition_version)` — отдельной миграцией.

## Удаление пользователя и tombstone

`PurgeUserData` удаляет обе таблицы оператором выше в транзакции библиотеки `go-tombstone` (она же ставит tombstone и отвечает OK на повтор). Библиотека пока каркас без миграций и API (FIN-110), поэтому в этом PR она не подключена. После выпуска:

1. `go get github.com/fin-assist/go-tombstone@vX.Y.Z`; в `MigrationSources()` — `{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations}` перед источником сервиса; `NoTombstones` заменяется библиотекой.
2. Функция удаления данных сервиса для обработчика `PurgeUserData` — `purgeSQL`.
3. `UpsertUserMetrics` для пользователя с tombstone — OK без записи, проверка в той же транзакции, что и запись (механизм защиты от воскрешения выбирает FIN-110). Внешнего ключа на таблицу библиотеки схема сервиса не заводит: живых пользователей `analytics` не знает.

## Объём

На пользователя — по строке снимка на период и режим: за год истории 12 месяцев + 4 квартала + 1 год, ×2 режима = 34 строки; за 10 лет — около 340. Снимок — единицы и десятки КиБ (списки категорий и платежей), сжимается TOAST. 10 тыс. пользователей с 2 годами истории — порядка 700 тыс. строк и единиц ГиБ: партиционирование и дополнительные индексы не нужны.

## Альтернативы

| Вариант | Почему не выбран |
|---|---|
| Метрика — колонка (`expense_minor`, …) | новая метрика — миграция; `details` у метрик разной формы; противоречит правилу расширяемости |
| Строка на метрику `(…, metric_id) → bytea/jsonb` | «полный набор метрик» — `DELETE` отсутствующих + upsert каждой, то есть несколько операторов и блокировок на снимок; версия всё равно на снимок периода; чтение 6.1 — десяток строк вместо одной. Даёт запросы по одной метрике и по `definition_version` в SQL — сейчас не нужны |
| `jsonb` (protojson) снимка | неизвестные поля protojson при разборе отбрасывает; больше по размеру и дороже разбор; имена enum в JSON — вторая форма той же схемы. Плюс — читаемость в `psql` и запросы по полям; если понадобятся, можно добавить сгенерированную проекцию |
| Удаление строк по `removed_periods` | после удаления не с чем сравнить версию: опоздавший старый снимок воскрешает период |
| Индекс `(user_id, period_type) WHERE snapshot IS NOT NULL` | первичный ключ уже даёт тот же префикс и порядок; меток мало |
| `user_id text` | см. открытые вопросы |
| PG `ENUM` для типа периода и режима | новое значение — `ALTER TYPE … ADD VALUE` вне транзакции файла миграции; номера proto проще и стабильнее |

## Открытые вопросы

1. **Откуда требуемая версия определения метрики** (FIN-6): композиция экрана в коде `analytics` (предложение, без таблицы) или данные от `bank-statement-data`. Контракт фиксирует только результат — `UNAVAILABLE`.
2. **`user_id uuid` или `text`.** Контракт называет ID непрозрачной строкой «сейчас UUIDv7», `user` выдаёт UUIDv7. `uuid` — 16 байт и отказ при не-UUID; цена — миграция, если формат ID сменится. Взят `uuid`.
3. **Одинаковая `calc_version` с разным содержимым.** Схема считает, что версия однозначно определяет расчёт (`<=` применяет повтор). Если `bank-statement-data` может прислать по одному ключу два разных снимка с одной версией, побеждает последний доставленный — нужно подтвердить, что так не бывает.
4. **Новый тип периода** (`week` в `docs/api.md` как пример расширения) потребует миграции CHECK на `period_type` и `period_key`. Это изменение контракта и так; если CHECK мешает, его можно ослабить до `period_type > 0`.
5. **Пояс во время пересчёта.** Пока идёт пересчёт после смены пояса (`recalculating`), рядом лежат снимки, посчитанные в старом и новом поясе; `time_zone` состояния — уже новый. Контракт это принимает (ответ с признаком `recalculating`), схема — тоже; подтвердить, что `bank-statement-data` удаляет ключи периодов, которых в новом поясе нет (`removed_periods`).
