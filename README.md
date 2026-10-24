# analytics

Выпуск версии сервиса и повторная публикация GitHub Release описаны в [docs/releasing.md](docs/releasing.md). История изменений — [CHANGELOG.md](CHANGELOG.md). История начинается с пустого `Unreleased`; версии и теги создаёт владелец после PR и post-merge CI. Публикация образа и deployment — отдельные этапы.

Сервис `analytics` «Понятные финансы»: хранит готовые персональные метрики, которые присылает `bank-statement-data` (`MetricsIngestService.UpsertUserMetrics`), и собирает из них экран «Аналитика» (`AnalyticsService.GetAnalytics`, `ListPeriods`; REST 6.1, 6.2). Сам метрики не считает. Контракт — [`fin-assist/contracts`](https://github.com/fin-assist/contracts): `docs/grpc-api.md` (7.3), `proto/finassist/analytics/v1/analytics.proto`, `docs/api.md` (6.1, 6.2, «Блоки»); модуль `github.com/fin-assist/contracts/gen/go`.

Создан из шаблона [`fin-assist/go-service-template`](https://github.com/fin-assist/go-service-template) ([FIN-117](https://finassist.youtrack.cloud/issue/FIN-117), перенос — [FIN-192](https://finassist.youtrack.cloud/issue/FIN-192)).

- [docs/schema.md](docs/schema.md) — схема БД: нормализованное хранение метрик (заголовок снимка, пропуски, строка на метрику), инварианты, версионированный upsert полного набора метрик, запросы ([FIN-113](https://finassist.youtrack.cloud/issue/FIN-113)).
- [docs/server.md](docs/server.md) — gRPC-сервер и интерцепторы, идентичность, таблица доступа, клиенты, транзакции, логи и метрики.
- [docs/config.md](docs/config.md) — переменные окружения.
- [docs/migrations.md](docs/migrations.md) — миграции и запросы на pgx ([FIN-115](https://finassist.youtrack.cloud/issue/FIN-115)).
- [AGENTS.md](AGENTS.md) — карта и правила для агентов, Code Review Rules.

## Автоматизация обновлений зависимостей

[Политика Renovate](docs/renovate-policy.md) (FIN-211): одна актуальная release/main, автомерж полностью ботских PR после обязательного CI, один повтор красного CI и закрытие с явным повтором через Dependency Dashboard. AndroidX/Compose остаются на ручной проверке. Исходные группировки, ограничения версий и расписания — `renovate-base.json`; generated target и quarantine — ветка `renovate-policy`. Активация требует отдельного токена и строгой защиты веток; по умолчанию выключена.

## Что ещё не подключено

- Обработчики `AnalyticsService`, `MetricsIngestService`, `UserDeletionService` и их строки в `AccessPolicy()` — отдельными задачами (FIN-5 и следующие).
- Tombstone — `NoTombstones` до выпуска `go-tombstone` ([FIN-110](https://finassist.youtrack.cloud/issue/FIN-110)); его миграции встают в `MigrationSources()` перед источником сервиса.
- Секрет `CLAUDE_CODE_OAUTH_TOKEN`, Claude GitHub App и Codex на репозитории — настраивает владелец.

## Раскладка

```
cmd/analytics/                     main: serve and migrate commands, listeners, shutdown
internal/app/                      assembly: identity, pools, server, wire() (handlers, clients, jobs), access table, migration sources
internal/app/apitest/              the whole service in-process for API tests
internal/domain/                   entities and invariants, no infrastructure imports
internal/<feature>/                usecases; interfaces of repositories and clients they need
internal/adapter/grpc/server/      gRPC server, interceptors, access table types, identity interfaces
internal/adapter/grpc/<feature>/   gRPC handlers: messages <-> usecases
internal/adapter/postgres/         pools, go-postgres adapter, repositories on pgx
internal/adapter/postgres/migrate/ migration runner (migratetest: PostgreSQL for tests)
internal/adapter/client/           clients of other services, service token source
internal/worker/                   background jobs: outbox dispatcher, scheduled jobs
internal/config/                   configuration from the environment
internal/reqctx/                   request-scoped values: request ID, caller, user, hints
internal/logx/                     slog with values from the context
internal/ops/                      /metrics, /healthz, /readyz, readiness
migrations/                        SQL migrations, embedded into the binary
deploy/systemd/                    example unit for the stand
```

Раскладка — по руководству go.dev «Organizing a Go module» (серверный проект), слои и интерфейсы на стороне потребителя — по go-clean-arch v4: сценарий в `internal/<feature>` объявляет интерфейсы нужных ему репозиториев и клиентов, адаптеры их реализуют, связывает всё `wire()`.

## Запуск

```bash
# PostgreSQL 18, migrations and the service in Docker (dev mode, docs/config.md)
make up
# the same service on the host against the compose PostgreSQL
make run
```

- **Образ** — `Dockerfile`: статический бинарь на distroless, пользователь `nonroot`. Один бинарь: `serve` в Deployment, `migrate up` в Job. Публикация в GHCR — FIN-148.
- **Стенд** (FIN-149) — тот же бинарь под systemd: пример unit-файла с секретами через `LoadCredential` — [deploy/systemd/analytics.service](deploy/systemd/analytics.service). Скрипт деплоя запускает `migrate up` перед перезапуском unit.
- **k3s** — манифесты в `fin-assist/infra` (FIN-147): ServiceAccount, projected token, headless Service, пробы `grpc`.

## Библиотеки и обновления

Транзакциями управляет [`go-postgres v0.1.1`](https://github.com/fin-assist/go-postgres) ([FIN-240](https://finassist.youtrack.cloud/issue/FIN-240)); `postgres.DB` сохраняет настройку пулов и прежний выбор primary/replica. `RunMapped` принимает обработчик конкретной операции из адаптера сервиса, включая отложенные ошибки COMMIT. Прикладные RPC analytics пока не зарегистрированы: production mappings и новые контрактные ошибки не добавлены. Правила — [docs/migrations.md](docs/migrations.md), «Нарушения ограничений и транзакции».

Общий код сервисов — отдельные Go-библиотеки из шаблона [`fin-assist/go-library-template`](https://github.com/fin-assist/go-library-template) ([FIN-118](https://finassist.youtrack.cloud/issue/FIN-118)): [`go-identity`](https://github.com/fin-assist/go-identity) (FIN-119), [`go-idempotency`](https://github.com/fin-assist/go-idempotency) (FIN-78), [`go-outbox`](https://github.com/fin-assist/go-outbox) (FIN-109), [`go-tombstone`](https://github.com/fin-assist/go-tombstone) (FIN-110).

- **Подключение** — по разделу «Внедрение» README библиотеки: `go get github.com/fin-assist/go-<имя>@vX.Y.Z`, источник миграций в `MigrationSources()` до источника сервиса, сборка в `wire()` — библиотека получает от сервиса транзакцию (`db.Tx`), логгер, реестр метрик, часы и настройки.
- **Версии** — semver-теги библиотеки, `go.mod` сервиса закрепляет версию; до первого прода — `v0`, несовместимое изменение — новый MINOR с записью «Несовместимо» в CHANGELOG библиотеки. Правила — `docs/releasing.md` в `fin-assist/go-library-template`.
- **Обновления приносит Renovate** (`renovate.json`): модули `github.com/fin-assist/*` (библиотеки и `contracts/gen/go`) — отдельный PR сразу после тега, остальные зависимости — один PR в неделю, версия Go — вручную. PR Renovate проходит CI сервиса и обрабатывается по [политике FIN-211](docs/renovate-policy.md). При несовместимом обновлении человеческий коммит в открытом PR переводит его на ручное сопровождение; если ботский PR уже закрыт после повторного падения, исправления сервиса идут отдельным PR с задачей YouTrack. Порядок миграции — по CHANGELOG библиотеки.
- **Проверить библиотеку до выпуска** — локально `go mod edit -replace github.com/fin-assist/go-<имя>=../go-<имя>`; `replace` в PR сервиса не попадает.

## Проверки

CI (`.github/workflows/ci.yml`):

| Job | Что |
|---|---|
| `go` | `go mod tidy` без изменений, `go vet`, три отдельных набора `unit`/`integration`/`api` с `-race`, PostgreSQL 18, проверкой фактического исполнения и порогом 80% statements каждого; combined дополнительно, отчёты в сводке и артефактах; тесты скриптов |
| `lint` | golangci-lint (`.golangci.yml`), в том числе `forbidigo` против обхода `finerr` и `depguard` против фреймворков, ORM и DI-контейнеров |
| `image` | сборка образа и запуск бинаря в нём |
| `migrations` (PR) | существующие миграции не изменены, squawk по новым |

Локально:

```bash
make test   # Docker for testcontainers, or TEST_DATABASE_URL=postgres://... to use your PostgreSQL
make lint
```

### Покрытие statements по трём наборам (FIN-238)

Для **каждого** набора `unit`, `integration` и `api` обязателен порог **80% statements всего production-кода сервиса**. Сбор — штатный Go coverage, проверка — `vladopajic/go-test-coverage/v2` **v2.20.1**. Общий результат `combined` показывает объединение профилей силами инструмента; он не компенсирует провал отдельного набора. Внешнего сервиса отчётности нет.

```bash
make collect-unit          # unit.out + unit.jsonl, without a percentage gate
make collect-integration   # real PostgreSQL required
make collect-api           # full application over its public API
make check-unit            # the corresponding 80% gate
make check-integration
make check-api
make cover-combined        # informational union of the three profiles
make cover                 # collect all three, then enforce each gate
```

Каждый прогон использует `-race -count=1 -timeout=8m -coverpkg=./...` и выбирает функции с префиксом `TestUnit`, `TestIntegration` или `TestAPI`. Новый тест обязательно получает один из этих префиксов; `make test-names` и CI отвергают остальные имена через штатный `go test -race -list`. Все наборы инструментируют один и тот же Go-модуль целиком, включая generated production-код и Go-файлы инфраструктуры `apitest`/`migratetest`; исключений пакетов или файлов ради порога нет. `_test.go` Go в production-профиль не включает. Чистые функции и правила — unit; PostgreSQL, транспорт, клиенты и фоновые процессы — integration; полная сборка приложения с вызовом публичного API — api. Прямой вызов handler не считается API-тестом.

`scripts/check-suite.sh` проверяет JSON-события Go: набор должен действительно выполнить хотя бы один тест и завершиться без failed/skipped тестов. Отсутствующий, пустой профиль, только заголовок и профиль без исполняемых statements отвергаются. Это проверка факта исполнения, не собственный расчёт процентов. Для всех coverage-прогонов `CI=true`: отсутствие PostgreSQL даёт ошибку, а не успешный skip. Инструмент округляет процент до одного знака перед проверкой порога; это согласованное поведение v2.20.1.

CI сохраняет `.out`, `.jsonl`, `*.coverage.txt` и `*.report.txt` как артефакт, публикует отдельные unit/integration/api/combined в сводке. Обязательный job `go` остаётся красным при провале любого отдельного набора. Fork PR не получают права записи комментариев.

Прежний критерий «80% изменённых исполняемых строк» заменён тремя порогами statements. Исторические проценты строк не сопоставимы с новыми. Проценты не доказывают полноту бизнес-сценариев.

Подробности классификации и исходные дефициты — [docs/testing.md](docs/testing.md).

Модули `github.com/fin-assist/*` берутся из git, минуя прокси (`GOPRIVATE`, задан в `Makefile` и CI). Для приватного — токен с чтением репозиториев fin-assist: локально `git config --global url."https://x-access-token:<token>@github.com/fin-assist/".insteadOf "https://github.com/fin-assist/"`, в CI — секрет `GO_MODULES_TOKEN`, в `docker build` — секрет `netrc`.
