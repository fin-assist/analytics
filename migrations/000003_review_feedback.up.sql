-- squawk-ignore-file prefer-bigint-over-int
CREATE TABLE recalculation_marker (
    user_id uuid PRIMARY KEY,
    requested_version integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE recalculation_marker IS 'Монотонный маркер пересчёта FIN-416.';
COMMENT ON COLUMN recalculation_marker.user_id IS 'Пользователь, сериализуемый общей tombstone guard.';
COMMENT ON COLUMN recalculation_marker.requested_version IS 'Максимальная подтверждённая целевая версия доставки true.';
COMMENT ON COLUMN recalculation_marker.created_at IS 'Время создания маркера';
ALTER TABLE period_snapshot ADD COLUMN time_zone text;
COMMENT ON COLUMN period_snapshot.time_zone IS 'Исходный IANA-пояс снимка из того же пакета производителя; NULL до новой доставки';

CREATE TABLE chart_cursor (
 user_id uuid NOT NULL REFERENCES user_data_state(user_id) ON DELETE CASCADE,
 transfer_mode smallint NOT NULL,
 revision text NOT NULL,
 page integer NOT NULL,
 token text NOT NULL UNIQUE,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,transfer_mode,revision,page)
);
COMMENT ON TABLE chart_cursor IS 'Непрозрачные указатели страниц месячной истории, принадлежащие пользователю';
COMMENT ON COLUMN chart_cursor.user_id IS 'Владелец, сверяемый с проверенным контекстом запроса';
COMMENT ON COLUMN chart_cursor.transfer_mode IS 'Режим переводов';
COMMENT ON COLUMN chart_cursor.revision IS 'Версия всей истории и исходного календарного дня';
COMMENT ON COLUMN chart_cursor.page IS 'Номер страницы от новейшего месяца, начиная с нуля';
COMMENT ON COLUMN chart_cursor.token IS 'Случайный непрозрачный указатель';
COMMENT ON COLUMN chart_cursor.created_at IS 'Время выдачи для удаления истёкших указателей';
