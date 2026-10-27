# gRPC, фоновые задания и сборка сервиса

Контракт межсервисного взаимодействия — [`fin-assist/contracts/docs/grpc-api.md`](https://github.com/fin-assist/contracts/blob/main/docs/grpc-api.md). FIN-257 подключает опубликованные механизмы вместо локальных копий. Публичные параметры, порядок интерцепторов, правила metadata, ошибок и retry описаны в [`go-grpc v0.2.0`](https://github.com/fin-assist/go-grpc/tree/v0.2.0) (FIN-330: v0.2.0 нужен для смешанной модели `ListPeriods` — вызов `bank-bff` без токена пользователя или с неверным заголовком `authorization` получает `INVALID_TOKEN`, а не системный вариант с `user_id` из запроса, FIN-299); lifecycle заданий — в [`go-worker v0.1.0`](https://github.com/fin-assist/go-worker/tree/v0.1.0).

## Ответственность сервиса

`internal/app` связывает `go-grpc/server` с identity/tombstone реализациями, logger, реестром метрик и настройками сервиса. `wire()` регистрирует прикладные обработчики и возвращает реальные исходящие клиенты и jobs; `AccessPolicy()` задаёт правила зарегистрированных методов по таблице grpc-api.md 3.3:

| Метод | Вызывающие | Модель |
|---|---|---|
| `analytics.AnalyticsService/*` | `bank-bff`, `assistant` | смешанная: `bank-bff` — с токеном пользователя (пользователь — проверенный `sub`; `user_id` запроса пуст или равен ему, иначе голый `INVALID_ARGUMENT`), `assistant` — системный вызов с обязательным `user_id` |
| `analytics.MetricsIngestService/*` | `bank-statement-data` | системный |
| `userdeletion.UserDeletionService/*` | `user` | системный |

Обработчики FIN-283: `UpsertUserMetrics`, `ListPeriods` (`internal/adapter/grpc/metrics`) и `PurgeUserData` (`DeletionServer` библиотеки `go-tombstone`). `GetAnalytics` (FIN-5) реализован в той же смешанной модели, что `ListPeriods`: пользователь из проверенного `sub`, системный `assistant` указывает `user_id`. Поддерживаются точный/последний период, оба режима переводов, `supports`, `analytics_order:b` (категории перед плитками), серверные тексты всех MVP-блоков. Неизвестные эксперименты игнорируются; несовместимые примитивы пропускаются. Устаревшие поля ответа 5–9 не заполняются: BFF читает `blocks`. Клиентов соседей и фоновых заданий, кроме обновления ключей identity, нет. Health и dev reflection принадлежат инфраструктурной сборке.

Обработчики остаются в `internal/adapter/grpc/<feature>/`, usecase — в `internal/<feature>/`. Данные контекста читаются через `github.com/fin-assist/go-grpc/reqctx`; локального `internal/reqctx` нет. Ошибки контракта создаются через `finerr`; внутренние bare status создаёт только библиотека `go-grpc/server`. Запрет обхода `finerr` в `.golangci.yml` сохраняется.

## Identity и удалённые пользователи

`AUTH_MODE=verify` (FIN-282) собирается одним конструктором `go-identity v0.2.0` `NewAssembly` (`internal/app/identity.go`, по образцу assistant): при `ENV=prod` — токены ServiceAccount Kubernetes (JWKS API-сервера по HTTPS с доверием только к `AUTH_KUBERNETES_CA_FILE`, отдельный API-токен, namespace и `sub`), исходящий токен — projected-файл `SERVICE_TOKEN_FILE`; при `ENV=stage` — короткие JWT стенда по файлам ключей `AUTH_STAGE_*`; в обоих — access JWT пользователя по ключам `user.GetSigningKeys` (`PEER_USER_ADDR`, повтор только этого метода, issuer/audience — `AUTH_USER_ISSUER`/`AUTH_USER_AUDIENCE`). `verify` с `ENV=dev` и любой неполный набор настроек — ошибка старта в `config.Load` и в `app.New`, без перехода на более слабый режим; ключи стенда в prod запрещены. Обновление кэшей ключей — задание `identity-keys` (`Assembly.Run`), соединение с `user` закрывается после остановки заданий. Второй цепочки аутентификации нет: сборка реализует `ServiceVerifier`, `UserVerifier` и `TokenSource` go-grpc. `insecure-dev` по-прежнему только при `ENV=dev` (FIN-275).

Удалённые пользователи — `go-tombstone v0.1.0`: `server.Deps.Tombstones` читает `tombstone_users` на primary без кэша на каждом пользовательском вызове (`TOKEN_REVOKED`, счётчик `finassist_tombstone_revoked_total`); пишущий сценарий `UpsertUserMetrics` берёт `Guard` в своей транзакции до записи и подтверждает опоздавшее событие удалённого пользователя без эффекта; `PurgeUserData` удаляет данные и ставит tombstone одной транзакцией (подробности — [schema.md](schema.md), «Удаление пользователя и tombstone»).

## Клиенты и задания

Общий dial, service token и request metadata — `github.com/fin-assist/go-grpc/client`. Адреса peers, retry-списки безопасных методов, таймауты и token source выбирает сервис. Прикладной адаптер конкретного peer появляется в `internal/adapter/client/<peer>/` только вместе с usecase; транспортную реализацию библиотеки не копируют.

Общий запуск и периодичность — `github.com/fin-assist/go-worker`. Сервис явно передаёт logger, clock и RestartDelay=5s, сохраняя прежнюю политику перезапуска независимо от default библиотеки. Реальные jobs размещаются в `internal/worker/<job>/`. Задания запускаются в каждом поде: единственность эффекта обеспечивает прикладная блокировка или `FOR UPDATE SKIP LOCKED`, а не сам runner.

## PostgreSQL и lifecycle

Сервис передаёт pool options в `go-postgres.OpenPools` v0.3.0. Локальный `postgres.DB` остаётся тонким адаптером менеджера транзакций и маршрутизации: `Writer` выбирает транзакцию/primary, `Reader` — транзакцию, primary для `x-read-primary` от `bank-bff` или реплику. Репозитории и domain mapping принадлежат сервису; подробнее — [migrations.md](migrations.md).

`cmd/analytics` владеет listeners и порядком shutdown: readiness/health перестают сообщать готовность (`ops.Readiness.Drain` — необратимо, HTTP `/readyz` отвечает 503 вместе с gRPC health `NOT_SERVING`, FIN-275), выдерживается `SHUTDOWN_DELAY`, gRPC graceful stop ограничен `SHUTDOWN_TIMEOUT`, затем останавливаются jobs и закрываются пулы. Вынос библиотек не меняет этот порядок или конфигурационные defaults.

## Наблюдаемость

Logger и Prometheus registry создаёт сервис и явно передаёт библиотекам. `go-grpc` не должен молча отключить прежние server/client metrics из-за nil metrics/registry; logger по умолчанию в библиотеке не заменяет сервисный logger. Имена и метки сохраняются: `grpc_server_handled_total{method,code,reason}`, `grpc_server_handling_seconds{method}`, `finassist_grpc_denied_total{method,caller,cause}`, `grpc_client_handled_total{peer,method,code}`, `grpc_client_handling_seconds{peer,method}`. Пользовательские идентификаторы и другие неограниченные значения в labels запрещены.

Метрики FIN-330:

| Метрика | Метки | Что |
|---|---|---|
| `finassist_incomplete_period_snapshots_total` | `period_type` (`month`, `quarter`, `year`) | периоды, которые `UpsertUserMetrics` применил с одним режимом переводов (grpc-api.md 7.3, 9); вместе с предупреждением в логе (тип и ключ периода, без пользователя) |
| `finassist_tombstone_revoked_total` | — | пользовательские вызовы удалённого пользователя, отклонённые `TOKEN_REVOKED` (`go-tombstone`) |
| `finassist_identity_rejected_total`, `finassist_identity_jwks_skipped_keys` | по README `go-identity` | отказы проверки токенов и пропущенные ключи JWKS (`go-identity`) |

`internal/logx` использует значения из `go-grpc/reqctx`. Логи не раскрывают токены, ключи, DSN и банковские данные; параметры маскирования и безопасного сообщения об ошибке задаёт контракт библиотеки. `/metrics`, `/healthz`, `/readyz` и readiness ping primary остаются в `internal/ops` и сборке сервиса. Библиотеки регистрируют метрики только в переданном реестре; process-wide logger/registry нет.

## Композиция GetAnalytics (FIN-5)

`internal/metrics/Screen` нормализует месяц → квартал/год, проверяет ключ и вычисляет `is_current`/`stale` по внедрённым часам. Репозиторий одним SQL читает default key, состояние, заголовок, пропуски, метрики и соседей: части ответа принадлежат одному MVCC-снимку, чтение следует `x-read-primary`. Операции повторно не агрегируются; базы/окна, суммы и пороги берутся из снимка. Сохранённый `definition_version` не сравнивается с текущим каталогом.

`Composer` строит блоки последовательно с отдельным контекстом и лимитом 250 мс на блок. Длительные циклы проверяют отмену; результат просроченного построения не используется. Ошибка деталей или число вне точного диапазона `Struct` (±(2^53−1)) даёт `unavailable` только своему блоку. Если ошибка построения произошла у всех запрошенных содержательных блоков — `INTERNAL`; отсутствие снимка или новой метрики само по себе не ошибка построения: экран с `coverage=no_data` и `unavailable` допустим. `has_data=false` не несёт остальные поля. Пустой результат фильтрации `supports` допустим.

Метрики композиции: `finassist_analytics_blocks_total{block_id}` и `finassist_analytics_blocks_unavailable_total{block_id}`; отношение счётчиков — доля недоступных блоков. Метка ограничена встроенными `tiles`, `monthly_chart`, `expense_categories`, `income_categories`, `regular_payments`, `notable_spending`, `bank_fees`, `small_frequent`, `balance`. Эксперимент, пользователь, категория и содержимое снимка в метки не попадают.

### Производные для представления

Сборка блоков включает арифметику над готовыми агрегатами одного сохранённого снимка, заданную существующими источниками: [api.md 6.1](https://github.com/fin-assist/contracts/blob/main/docs/api.md#61-получить-аналитику), [calc.md §1, §5, §10](https://github.com/fin-assist/contracts/blob/main/docs/calc.md), [analytics.proto, MonthlyPoint/MonthlyChart](https://github.com/fin-assist/contracts/blob/main/proto/finassist/analytics/v1/analytics.proto). Это доля `(I−E)/I` столбца баланса (нет при `I≤0`), процент текстового сравнения по присланному сравнению, кратность заметной траты по присланному «обычно» и сумма скрытых строк регулярных платежей. Проценты текста округляются `floor(x+0.5)` при любом знаке по calc.md §1; доли столбцов не округляются. Модель MonthlyPoint передаёт E/I, отдельного значения доли в действующем proto нет.

Analytics не выбирает операции, не меняет transfer_mode/базу, не распознаёт платежи и не пересчитывает пороги или прогноз. Доля сводки берётся из `BalanceDetails.share_of_income`, E/I столбца — из MonthlyPoint того же снимка. Вариант нового определения с другой финансовой семантикой потребует согласованного producer/contract/composition; смена `definition_version` сама по себе не меняет отображение старого снимка. Новая финансовая политика FIN-5 не вводится.

Неизвестная/отсутствующая coverage точки не подменяется partial: поле скрывается по grpc-api.md §8. Повреждение monthly_chart скрывает график баланса, сохраняя исправные summary/caption. Заголовок наблюдений выводится только при поддержке insight_card. Название периода прогноза берётся из нормализованного ключа периода, включая время пересчёта после смены часового пояса.
