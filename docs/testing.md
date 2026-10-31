# Проверки и покрытие (FIN-238 / FIN-257 / FIN-330)

analytics использует выпущенные `go-grpc v0.2.0`, `go-migrate v0.1.1`, `go-worker v0.1.0`, `go-postgres v0.3.0`, `go-identity v0.2.0` и `go-tombstone v0.1.0`. FIN-330 реализует `UpsertUserMetrics`, `ListPeriods`, `PurgeUserData`, production identity и tombstones; `GetAnalytics` реализован в FIN-5.

| Набор | Выбор Go | Что проверяется |
|---|---|---|
| Unit | `^(Test\|Fuzz)Unit` | Каждое правило `domain.Upsert.Validate` и fuzz ключа периода/пояса; сценарий `internal/metrics` на фейках (порядок guard → состояние → периоды, отказ до транзакции, удалённый пользователь, откат, отчёт о периоде с одним режимом после фиксации); перевод proto → domain и отображение в строки (каждое поле, отказы `q`/сумм/моментов, границы `smallint`); обработчики (голый `INVALID_ARGUMENT`, смешанная модель `ListPeriods`, `VALIDATION_ERROR` неизвестного типа); таблица доступа, метрика неполных периодов; конфигурация identity (`AUTH_*`, prod/stage/dev); ветки сборки identity без сети; CLI, routing, lifecycle |
| Integration | `^(Test\|Fuzz)Integration` | PostgreSQL 18: схема, версии по ключу, правило удаления по бо́льшей версии двух режимов и растущей метки, round trip снимка, конкурентная доставка, планы запросов по первичным ключам; репозиторий только в транзакции; сценарий с реальным `go-tombstone`: удаление и запись сериализуются guard (оба порядка), конкурентные пакеты одного пользователя; identity prod (TLS JWKS, CA, API-токен, gRPC `GetSigningKeys` на bufconn) и stage (файлы ключей), ошибки конфигурации; up/down/up миграций, CLI `migrate` с источником `tombstone` |
| API | `^(Test\|Fuzz)API` | Полная сборка (`apitest`): `UpsertUserMetrics` — применение, повтор той же версии, старая версия (`skipped_stale_periods`), удаление, каждая ошибка правил записи, чужие вызывающие; `ListPeriods` — пользователь, свой/чужой `user_id`, системный вызов `assistant` с `user_id` и без, пустой список, неизвестный тип, чужие вызывающие, `bank-bff` без токена и с неверным `authorization`; `PurgeUserData` — только `user`, повтор, данные другого пользователя целы, опоздавший пакет без эффекта, `TOKEN_REVOKED`; CLI `serve` с `verify` в prod и stage на настоящих TCP/HTTPS (токены, маскирование debug-лога, отказ неполной настройки); operational API |

Тесты схемы вызывают PostgreSQL напрямую и остаются integration. Production-чтение `GetAnalytics` живёт в `internal/adapter/postgres/analytics.go`; прежние round trip-тесты используют именно его SQL и сканирование.

## PostgreSQL и общий знаменатель

`internal/testdb` выбирает PostgreSQL18 из `TEST_DATABASE_URL` либо testcontainer. Публичный `go-migrate/migratetest.DSN(t, adminDSN)` создаёт отдельную БД и bounded cleanup; `CheckUpDownUp(t, dsn, source)` проверяет переданный реальный источник миграций. Ownership разделён: сервер принадлежит сервисному entrypoint, изолированная БД — DSN helper, источники SQL — приложению.

`internal/testdb` и `internal/app/apitest` остаются production-пакетами в смысле Go coverage и входят в один полный знаменатель всех трёх наборов `-coverpkg=./...`, без исключений. Unit не обращается к БД/сети. Все collectors используют `-race -count=1 -timeout=8m`; execution/profile guards и independent total80 неизменны, combined только справочный. Команды и pinned analyzer v2.20.1 — [README](../README.md#покрытие-statements-по-трём-наборам-fin-238).

Текущие native результаты FIN-257 публикуются в CI artifact `coverage`. Локальный прогон Go1.26.0/darwin с PostgreSQL18 через `TEST_DATABASE_URL` отличается от CI на Linux/Docker: контейнерная ветка testdb остаётся в знаменателе локально. Достижение трёх порогов подтверждает CI текущего SHA, а не импорт библиотек или исторический процент.

## Локальный замер FIN-5 на 34952e2, 27.10.2026 (Go1.27.1 darwin/amd64, PostgreSQL18.6)

`make test`, `make lint` и `make cover` прошли. Штатный go-test-coverage v2.20.1, все 1956 production statements в знаменателе каждого набора, без исключений; `-race -count=1 -timeout=8m` и внешний таймаут каждого процесса. Скриптовые тесты: 133, Python3.12. Combined — справочно.

| Набор | Statements | Порог 80% |
|---|---|---|
| unit | 93.2% (1823/1956) | Выполнен |
| integration | 87.9% (1720/1956) | Выполнен |
| api | 87.1% (1704/1956) | Выполнен |
| combined | 96.9% (1895/1956) | Справочно |

## Локальный замер FIN-330 25.10.2026 (Go1.26.0 linux/amd64, PostgreSQL 18 через `TEST_DATABASE_URL`)

| Набор | Statements | Порог 80% |
|---|---|---|
| unit | 90.2% (896/993) | Выполнен |
| integration | 86.5% (859/993) | Выполнен |
| api | 82.5% (819/993) | Выполнен |
| combined | 97.5% (968/993) | Справочно |

Подтверждение — CI текущего SHA (контейнерная ветка `internal/testdb` локально не исполняется и остаётся в знаменателе).

## Исторический локальный замер до FIN-257 24.10.2026 (Go1.26.0 darwin/amd64)

| Набор | Statements | Выполнение тестов | Порог 80% |
|---|---|---|---|
| unit | 35.1% | Успех, PostgreSQL не нужен | Не выполнен |
| integration | 68.9% | Успех на PostgreSQL 18 | Не выполнен |
| api | 27.4% | Успех на PostgreSQL 18 | Не выполнен |
| combined | 86.2% | Информационное объединение | Отдельные наборы не заменяет |

1015 statements во всех трёх профилях. Это замер после актуализации на FIN-240 и классификации с `-race -count=1 -timeout=8m -coverpkg=./...`, а не подтверждение завершения FIN-238. Старые метрики строк сюда не переносились. Целевой 80% применяется к каждому набору; код и тестовая инфраструктура из знаменателя не исключались. Недостаток покрытия не скрывается и не заполняется искусственными тестами ради процентов.

FIN-5 добавляет синтетический typed-snapshot fixture `testdata/analytics.json`. Матрица `TestIntegrationGetAnalyticsSnapshotTransport` проходит через реальный bufconn transport/usecase/PostgreSQL; `TestAPIGetAnalyticsScreenContract` — через полную production-сборку с identity/access/tombstones. Проверяются оба режима и готовые actions/selection, все MVP-примитивы/состояния, supports/exp, точный период и навигация, ранний месяц, stale и окна блокировок, старые версии определений, отрицательные суммы/доли, безопасный числовой диапазон, частичный отказ JSON и пустые данные. Unit отдельно проверяет часы/DST/границы, обработчики, отображение строк, таймаут блока, полный отказ и registry-конфликты. Процент покрытия не заменяет эти сценарии. Производитель расчётов и реальная цепочка импорта проверяются отдельно в FIN-355/357/362.

После ревью FIN-5 добавлены регрессии timezone-rebuild caption, независимости summary баланса от повреждённого monthly_chart, округления текста (half-up при отрицательном значении, без −0%), orphan heading, неизвестной coverage и small_frequent по calc.md §10. В integration/API второй пользователь имеет собственный более поздний снимок; default, навигация и значения остаются пользовательскими, явный чужой период даёт no_data. Default SQL проверяется вместо прежнего test-only latest-запроса, в том числе через границу года и generic plan. Конкурентная API-проверка сопоставляет параметры, state, snapshot header, metric и navigation одной версии при 64 заменах и 128 чтениях.

Календарные регрессии сравнивают все серверные строки при пересчёте между исходным и новым поясом (ISO timestamps сравниваются как моменты): MSK↔UTC, New York↔Tokyo, январь с базой декабря прошлого года, Berlin с историческими DST-смещениями. Дополнительно проверяются квартальный peak/alt по MonthlyPoint.month, неизменность snapshot/range, обе стороны timezone-rebuild в integration/API и сохранение metric action при отсутствующем/незнакомом фильтре крупнейшей small_frequent-группы.

DST-регрессии отдельно проверяют реальный исходный IANA в завершённом снимке и безопасное отсутствие дневной точности при пересчёте: Berlin март с операцией 30.03 00:30 после перехода, октябрь с data_to в конце 28-го/31-го, Q4 с 31.12 23:59:59 и London-год с одинаковыми UTC-смещениями обеих январских границ, но иной летней датой. Во всех трёх наборах проверяются период из ключа, явное partial/recalculating, отсутствие выдуманного дня barNote и неизменность range/bounds; integration/API делают запись и чтение обоих режимов на PostgreSQL, включая неизменность абсолютных action filters.

Порядок примечаний в пустых текстах проверяется точными строками для none у expense_categories, income_categories и bank_fees: завершённое «за <период> нет» предшествует «неполные данные»/«идёт пересчёт»/«сохранённый расчёт». Unit включает обычный точный календарь, integration/API — существующий none-сценарий с четырьмя сочетаниями partial/complete и recalculating/retained. Для этой последующей текстовой правки локально выполнены целевые unit/integration/API и lint; полный замер выше относится к указанному 34952e2, новый SHA перепроверяется CI.

FIN-391: unit-регрессия проводит три несовместимые пары `month+2026`, `quarter+2026`, `month+2026-Q3` через настоящий Screen.Get и gRPC-handler с fake storage, затем проверяет штатный `finerr.ToREST` из pinned contracts/gen/go v0.7.0. До исправления все три ошибки имели gRPC InvalidArgument, но отвергались ToREST из-за пустого BadRequest.reason. Component integration использует отдельную сборку adapter/usecase/реального PostgreSQL и bufconn; API независимо использует app/apitest с production interceptors/access/tombstones. Проверены поле date, INVALID_FORMAT, HTTP400 registry mapping, неизвестный ListPeriods.period и корректные ключи с разрешённым укрупнением.


FIN-398/400/403: синтетический `testdata/all_time.json` содержит точные покрытые дни декабря/января/октября,
нулевой полный январь и восемь gap-месяцев, оба режима и только семь разрешённых семейств метрик.
Unit отдельно проверяет ключ/диапазон/точность пояса, v1→v2 без изменения сумм/статусов/фильтров,
zero/negative/safe-max/locked, ранний и stale режимы, empty groups и каноническую валидацию supports.
Независимый adapter/usecase/PostgreSQL transport проверяет хранение/список обоих режимов, stale replay/removal
и состояние до первого снимка; полная API-сборка независимо проверяет authentication/tenant/tombstone,
выбор версии и действия covered/zero/gap. Обычный текущий месяц с прогнозом и пустой календарный период
проверены на integration/API уровнях отдельно от all_time. Общие тесты транзакционной конкурентности,
отказа БД и per-block timeout продолжают действовать: схема/SQL/wiring/политика доступа не менялись.

FIN-411 после объединения PR51: `TestAPICombinedCalendarAllTimeAndRollingKeepTheirRangesAndFacts`
пишет календарные, all_time и rolling снимки одного пользователя в один PostgreSQL и читает оба режима с
`tile_group@1/@2`. Проверяются точные singleton-ключи, чужие ключи, диапазоны/навигация, неизменность финансовых
значений, статусов, locks и действий при переключении групп. `TestUnitPeriodSelectionsKeepCalendarAndSingletonSemantics`
проверяет все шесть типов через общий интерфейс стратегий. All-time state fallback/retained-range регрессии
предшествующего PR сохранены; assistant chips запрещает BFF, RPC-чтение assistant остаётся разрешённым по контракту.
