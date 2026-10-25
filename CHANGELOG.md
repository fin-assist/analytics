# История изменений

## [Unreleased]

### Изменено

- При остановке HTTP `/readyz` отвечает 503 вместе с gRPC health `NOT_SERVING` до паузы `SHUTDOWN_DELAY`, и после `Drain` цикл готовности не может сообщить готовность; `app.New` отказывает в `AUTH_MODE=insecure-dev` вне `ENV=dev` и без `config.Load`; `DB_MAX_CONNS` вне диапазона int32 — ошибка конфигурации вместо молчаливого усечения; тест закрытия пулов ждёт выхода backend PostgreSQL (FIN-275).

- Общая инфраструктура подключена выпущенными `go-grpc`, `go-migrate`, `go-worker` v0.1.0 и `go-postgres` v0.2.0 вместо локальных копий; сохранены CLI, health/metrics, shutdown, реальные миграции и schema tests analytics. Добавлены проверки сборки, cleanup и operational API без реализации бизнес-RPC (FIN-257).

- Транзакции используют `go-postgres v0.1.1`: единое управление вложенностью и ошибками запросов/COMMIT, отказ при чужом транзакционном контексте. Настройка пулов и выбор primary/replica сохранены; API и схема analytics не менялись (FIN-240).
