ALTER TABLE user_data_state ADD COLUMN data_revisions jsonb;
COMMENT ON COLUMN user_data_state.data_revisions IS
 'Объект calc_version и revisions с открытыми кодами и точными int64. Подтверждение известно только при совпадении версии строки и отсутствии более новых заголовков/удалений пользователя; после записи старым образом автоматически неизвестен. NULL означает неизвестную историю.';
ALTER TABLE period_snapshot ADD COLUMN data_revisions jsonb;
COMMENT ON COLUMN period_snapshot.data_revisions IS
 'Объект calc_version и revisions именно этого снимка периода и режима. Несовпадение stamp с версией финансовой строки после rollback/rollforward означает unknown; у прежних снимков NULL, подстановка из состояния запрещена.';
