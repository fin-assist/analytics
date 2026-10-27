ALTER TABLE user_data_state ADD COLUMN data_revisions jsonb;
COMMENT ON COLUMN user_data_state.data_revisions IS
 'Известные ревизии исходных данных по открытым кодам. NULL или пустой объект означают неизвестную историю; значения относятся к этому состоянию.';
ALTER TABLE period_snapshot ADD COLUMN data_revisions jsonb;
COMMENT ON COLUMN period_snapshot.data_revisions IS
 'Ревизии именно этого снимка периода и режима, часть payload calc_version. Не заменяются более новым состоянием пользователя; у прежних снимков остаются неизвестными.';
