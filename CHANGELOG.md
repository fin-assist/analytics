# История изменений

## [Unreleased]

### Добавлено

- `MetricsIngestService.UpsertUserMetrics`: проверка всего пакета по правилам «Инвариантов в коде сервиса» (`internal/domain`, голый `INVALID_ARGUMENT`, в том числе для `METRIC_STATUS_UNAVAILABLE` от производителя и периода и в снимках, и в удалениях), одна транзакция READ COMMITTED: tombstone guard, состояние, заголовки и метки, пропуски и метрики применённых ключей; удаление периода сравнивается с бо́льшей из версий двух режимов, версия метки только растёт; `lock.text` производителя игнорируется; период с одним режимом — предупреждение и `finassist_incomplete_period_snapshots_total{period_type}`. `AnalyticsService.ListPeriods` (смешанная модель: `bank-bff` с токеном пользователя, `assistant` с `user_id`) и `UserDeletionService.PurgeUserData` (`go-tombstone`); строки `AccessPolicy()` по grpc-api.md 3.3. `GetAnalytics` — FIN-5, отвечает `UNIMPLEMENTED` (FIN-283).
- `AUTH_MODE=verify` через `go-identity v0.2.0` `NewAssembly`: `ENV=prod` — ServiceAccount Kubernetes, `ENV=stage` — файлы ключей стенда, JWT пользователя — по ключам `user.GetSigningKeys`; новые переменные `AUTH_USER_*`, `AUTH_KUBERNETES_*`, `AUTH_STAGE_*`, обязательный `PEER_USER_ADDR`; `verify` при `ENV=dev` и неполные настройки — ошибка старта. `go-tombstone v0.1.0` вместо `NoTombstones`: источник миграций `tombstone` перед сервисным, `TOKEN_REVOKED` удалённому пользователю на любой вызов (FIN-282).

### Изменено

- `go-grpc` v0.1.0 → v0.2.0: вызов смешанного метода от `bank-bff` без проверенного токена пользователя получает `INVALID_TOKEN`, а не системный вариант с `user_id` из запроса (FIN-299, нужен для `ListPeriods`). Предложенные в FIN-113 запросы записи и отображение снимка перенесены из тестов `migrations/` в репозиторий `internal/adapter/postgres/metrics.go`, тесты схемы — в тот же пакет; `ListPeriods` читает пояс и периоды одним оператором (FIN-283).
- `make test-names` проверяет и fuzz-цели (`FuzzUnit…`/`FuzzIntegration…`/`FuzzAPI…`, seed-корпус — в своём наборе) и отклоняет `Example…`; сборщики выбирают `^(Test|Fuzz)<Suite>` (FIN-290, перенос go-service-template#26).

- Документация приведена к шаблону и решениям: правило FIN-195 о нарушениях ограничений и полное правило схемы FIN-194 в `AGENTS.md`, актуальный статус `go-tombstone` в `docs/schema.md`, описание `GRPC_MAX_RECV_MSG_SIZE` по контракту analytics, ссылки README на задачи неподключённых RPC и identity/tombstone. Поведение сервиса не менялось (FIN-280).

- При остановке HTTP `/readyz` отвечает 503 вместе с gRPC health `NOT_SERVING` до паузы `SHUTDOWN_DELAY`, и после `Drain` цикл готовности не может сообщить готовность; `app.New` отказывает в `AUTH_MODE=insecure-dev` вне `ENV=dev` и без `config.Load`; `DB_MAX_CONNS` вне диапазона int32 — ошибка конфигурации вместо молчаливого усечения; тест закрытия пулов ждёт выхода backend PostgreSQL (FIN-275).

- `docs/schema.md`: решённые открытые вопросы о повторе одной `calc_version`, удалении периодов после смены пояса, порядке выкатки нового поля метрики и `UNAVAILABLE`/`lock.text` от производителя заменены ссылками на правила контракта (`docs/grpc-api.md` 7.3, contracts#60); метка удалённого периода связана с правилом версий удаления из контракта (удаление сравнивается с бо́льшей из версий двух режимов и с существующей меткой, версия метки только растёт); названы тест `TestIntegrationRemovedPeriodStaysRemovedForOlderSnapshots`, чьё ожидание переворачивается в FIN-283, и правило, которое там сохраняется. Только документация: код, схема и API analytics не менялись (FIN-284, FIN-283).

- Общая инфраструктура подключена выпущенными `go-grpc`, `go-migrate`, `go-worker` v0.1.0 и `go-postgres` v0.2.0 вместо локальных копий; сохранены CLI, health/metrics, shutdown, реальные миграции и schema tests analytics. Добавлены проверки сборки, cleanup и operational API без реализации бизнес-RPC (FIN-257).

- Транзакции используют `go-postgres v0.1.1`: единое управление вложенностью и ошибками запросов/COMMIT, отказ при чужом транзакционном контексте. Настройка пулов и выбор primary/replica сохранены; API и схема analytics не менялись (FIN-240).
