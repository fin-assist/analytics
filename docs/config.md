# Конфигурация

Всё — переменные окружения (`internal/config`). Секрет можно передать файлом: `VAR_FILE=/путь` вместо `VAR` (systemd `LoadCredential`, смонтированный Secret Kubernetes); заданы оба — ошибка запуска. Ошибки конфигурации выводятся все сразу и не содержат значений секретов.

Шаблон не опирается на Kubernetes: адреса других сервисов, DSN и файл токена — обычные настройки, одинаковые для k3s и стенда под systemd.

## Как меняется

Таблицы ниже **генерируются** из списка `Vars` в `internal/config/vars.go` — там у каждой переменной имя, значение по умолчанию, признак секрета и описание; код берёт значения по умолчанию оттуда же. Новая переменная или новое значение по умолчанию:

1. строка в `Vars`, чтение в `load` / `loadMigrate`;
2. `go test ./internal/config -run TestUnitDocIsCurrent -update` — переписывает таблицы между маркерами.

Руками блоки между маркерами не правятся. CI (`go test`) падает, если таблица устарела (`TestUnitDocIsCurrent`), если код читает необъявленную переменную или объявленная нигде не читается (`TestUnitVarsMatchWhatLoadReads`).

## `serve`

<!-- generated:config-serve:start (go test ./internal/config -run TestUnitDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `SERVICE_NAME` | — (обязательна) | имя сервиса: логи, `application_name`, токен в dev |
| `ENV` | `prod` | `dev`, `stage` или `prod`. Reflection включён только при `dev` |
| `AUTH_MODE` | `verify` | `verify` — go-identity `NewAssembly`: JWT пользователя по ключам user, сервисные токены — ServiceAccount через JWKS Kubernetes при `ENV=prod` или JWT стенда по файлам ключей при `ENV=stage`; при `ENV=dev` не допускается. `insecure-dev` — токены `dev:<имя>` без проверки, только при `ENV=dev` |
| `AUTH_USER_ISSUER` | — | доверенный issuer access JWT user; обязателен при verify |
| `AUTH_USER_AUDIENCE` | — | доверенная audience access JWT user; обязательна при verify |
| `AUTH_KUBERNETES_ISSUER` | `https://kubernetes.default.svc.cluster.local` | issuer токенов ServiceAccount; сверить с настройкой кластера |
| `AUTH_KUBERNETES_JWKS_URL` | `https://kubernetes.default.svc/openid/v1/jwks` | HTTPS-адрес ключей ServiceAccount без query, fragment и credentials |
| `AUTH_KUBERNETES_CA_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | CA Kubernetes API |
| `AUTH_KUBERNETES_API_TOKEN_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | API-токен для чтения JWKS; отдельный от finassist-internal |
| `AUTH_KUBERNETES_NAMESPACE` | `finassist` | доверенный namespace ServiceAccount |
| `AUTH_STAGE_ISSUER` | — | issuer сервисных JWT стенда, общий для сервисов стенда; обязателен при verify и `ENV=stage`, запрещён при `ENV=prod` |
| `AUTH_STAGE_SIGNING_KEY_FILE` | — | файл собственного PKCS#8 PEM Ed25519 ключа для исходящих JWT стенда (`kid` = `sub` = `SERVICE_NAME`); читается при старте; обязателен при verify и `ENV=stage`, запрещён при `ENV=prod` |
| `AUTH_STAGE_PUBLIC_KEYS_FILE` | — | JWKS публичных Ed25519 ключей сервисов стенда (`kid` — имя сервиса); читается при старте; обязателен при verify и `ENV=stage`, запрещён при `ENV=prod` |
| `SERVICE_TOKEN_FILE` | — | файл с projected-токеном ServiceAccount (audience `finassist-internal`) для исходящих вызовов; обязателен при verify и `ENV=prod`, в stage не используется |
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary (`-rw`) |
| `DB_REPLICA_DSN` / `_FILE` | — | DSN реплик (`-ro`); пусто — чтение тоже с primary. На стенде оба указывают на один PostgreSQL |
| `DB_MAX_CONNS` | `10` | размер каждого пула |
| `GRPC_ADDR` | `:9090` | gRPC |
| `GRPC_MAX_RECV_MSG_SIZE` | `4194304` | байт, предел входящего сообщения. Самый большой вход analytics — `UpsertUserMetrics`, его вызовы не больше 4 МиБ (grpc-api.md 7.3); поднимать без изменения контракта не нужно |
| `HTTP_ADDR` | `:9091` | `/metrics`, `/healthz`, `/readyz` |
| `PEER_<NAME>_ADDR` | — | адрес другого сервиса: `PEER_USER_ADDR` → `cfg.Peers["user"]` (обязателен при verify: ключи JWT пользователя, `user.GetSigningKeys`). k3s: `dns:///user.finassist.svc.cluster.local:9090`; стенд: `dns:///127.0.0.1:9092` |
| `LOG_LEVEL` | `info` | `debug` добавляет в строку вызова metadata и запрос с замаскированными токенами |
| `LOG_FORMAT` | `json` | `json` или `text` |
| `SHUTDOWN_DELAY` | `5s` | после `NOT_SERVING` сервер ещё принимает вызовы, пока балансировщики не уберут под |
| `SHUTDOWN_TIMEOUT` | `20s` | сколько ждать завершения вызовов; оставшиеся обрываются (потоки ответа клиент переоткроет) |
<!-- generated:config-serve:end -->

## `migrate`

<!-- generated:config-migrate:start (go test ./internal/config -run TestUnitDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary, роль сервиса. Миграции никогда не идут на реплику |
| `MIGRATE_LOCK_TIMEOUT` | `5s` | `lock_timeout`: DDL, который не дождался блокировки занятой таблицы, падает, а не держит очередь запросов сервиса за собой |
| `MIGRATE_STATEMENT_TIMEOUT` | `15m` | `statement_timeout` каждой команды |
| `MIGRATE_WAIT_TIMEOUT` | `15m` | сколько ждать, пока другой запуск миграций того же источника закончит |
<!-- generated:config-migrate:end -->

Как действуют таймауты миграций — [migrations.md](migrations.md), «Таймауты».
