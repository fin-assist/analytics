# История изменений

## [Unreleased]

### Изменено

- Общая инфраструктура подключена выпущенными `go-grpc`, `go-migrate`, `go-worker` v0.1.0 и `go-postgres` v0.2.0 вместо локальных копий; сохранены CLI, health/metrics, shutdown, реальные миграции и schema tests analytics. Добавлены проверки сборки, cleanup и operational API без реализации бизнес-RPC (FIN-257).

- Транзакции используют `go-postgres v0.1.1`: единое управление вложенностью и ошибками запросов/COMMIT, отказ при чужом транзакционном контексте. Настройка пулов и выбор primary/replica сохранены; API и схема analytics не менялись (FIN-240).
