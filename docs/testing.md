# Проверки и покрытие (FIN-238 / FIN-257)

Каркас analytics использует выпущенные `go-grpc`, `go-migrate`, `go-worker` v0.1.0 и `go-postgres` v0.2.0. `AccessPolicy()` и `wire()` не регистрируют прикладных RPC, клиентов или jobs. Перенос инфраструктуры не реализует отсутствующий бизнес-функционал и не включает production identity/tombstones.

| Набор | Выбор Go | Что проверяется |
|---|---|---|
| Unit | `^TestUnit` | Изолированные конфигурация, CLI validation/exit codes, PostgreSQL routing/foreign transaction, порядок assembly/cleanup и shutdown через consumer interfaces без БД и сети |
| Integration | `^TestIntegration` | Реальный PostgreSQL18: схема analytics, versioned upsert и snapshot round trip, up/down/up migration source, CLI up/version1/down/up/force1, транзакции/constraint mappings; настоящий transport, readiness и lifecycle сборки |
| API | `^TestAPI` | Полная сборка с реальными миграциями analytics: gRPC health/reflection и отказ неизвестным RPC, CLI deployment и настоящие TCP/HTTP `/healthz`, `/readyz`, `/metrics`, выход при неверной конфигурации/bind failure, закрытие listeners и readiness при потере/восстановлении PostgreSQL |

Проверки схемы вызывают PostgreSQL напрямую и остаются integration; тестовые бизнес-RPC/SQL/jobs в runtime не добавлены. Существующие schema SQL и regressions транзакционного адаптера сохранены. Самостоятельные транспортные/runner/worker сценарии принадлежат тестам библиотек, а сервис проверяет их реальное подключение и собственный lifecycle.

## PostgreSQL и общий знаменатель

`internal/testdb` выбирает PostgreSQL18 из `TEST_DATABASE_URL` либо testcontainer. Публичный `go-migrate/migratetest.DSN(t, adminDSN)` создаёт отдельную БД и bounded cleanup; `CheckUpDownUp(t, dsn, source)` проверяет переданный реальный источник миграций. Ownership разделён: сервер принадлежит сервисному entrypoint, изолированная БД — DSN helper, источники SQL — приложению.

`internal/testdb` и `internal/app/apitest` остаются production-пакетами в смысле Go coverage и входят в один полный знаменатель всех трёх наборов `-coverpkg=./...`, без исключений. Unit не обращается к БД/сети. Все collectors используют `-race -count=1 -timeout=8m`; execution/profile guards и independent total80 неизменны, combined только справочный. Команды и pinned analyzer v2.20.1 — [README](../README.md#покрытие-statements-по-трём-наборам-fin-238).

Текущие native результаты FIN-257 публикуются в CI artifact `coverage`. Локальный прогон Go1.26.0/darwin с PostgreSQL18 через `TEST_DATABASE_URL` отличается от CI на Linux/Docker: контейнерная ветка testdb остаётся в знаменателе локально. Достижение трёх порогов подтверждает CI текущего SHA, а не импорт библиотек или исторический процент.

## Исторический локальный замер до FIN-257 24.10.2026 (Go1.26.0 darwin/amd64)

| Набор | Statements | Выполнение тестов | Порог 80% |
|---|---|---|---|
| unit | 35.1% | Успех, PostgreSQL не нужен | Не выполнен |
| integration | 68.9% | Успех на PostgreSQL 18 | Не выполнен |
| api | 27.4% | Успех на PostgreSQL 18 | Не выполнен |
| combined | 86.2% | Информационное объединение | Отдельные наборы не заменяет |

1015 statements во всех трёх профилях. Это замер после актуализации на FIN-240 и классификации с `-race -count=1 -timeout=8m -coverpkg=./...`, а не подтверждение завершения FIN-238. Старые метрики строк сюда не переносились. Целевой 80% применяется к каждому набору; код и тестовая инфраструктура из знаменателя не исключались. Недостаток покрытия не скрывается и не заполняется искусственными тестами ради процентов.

Прикладные `AnalyticsService`, `MetricsIngestService` и `UserDeletionService` остаются отдельными задачами. Зелёное инфраструктурное покрытие не подтверждает реализацию этих RPC и их контрактных сценариев.
