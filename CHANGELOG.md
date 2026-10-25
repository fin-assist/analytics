# История изменений

## [Unreleased]

### Изменено

- Документация приведена к шаблону и решениям: правило FIN-195 о нарушениях ограничений и полное правило схемы FIN-194 в `AGENTS.md`, актуальный статус `go-tombstone` в `docs/schema.md`, описание `GRPC_MAX_RECV_MSG_SIZE` по контракту analytics, ссылки README на задачи неподключённых RPC и identity/tombstone. Поведение сервиса не менялось (FIN-280).

- Общая инфраструктура подключена выпущенными `go-grpc`, `go-migrate`, `go-worker` v0.1.0 и `go-postgres` v0.2.0 вместо локальных копий; сохранены CLI, health/metrics, shutdown, реальные миграции и schema tests analytics. Добавлены проверки сборки, cleanup и operational API без реализации бизнес-RPC (FIN-257).

- Транзакции используют `go-postgres v0.1.1`: единое управление вложенностью и ошибками запросов/COMMIT, отказ при чужом транзакционном контексте. Настройка пулов и выбор primary/replica сохранены; API и схема analytics не менялись (FIN-240).
