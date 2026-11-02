-- squawk-ignore-file prefer-bigint-over-int
CREATE TABLE recalculation_marker (
    user_id uuid PRIMARY KEY,
    requested_version integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE recalculation_marker IS 'Монотонный маркер пересчёта FIN-416.';
COMMENT ON COLUMN recalculation_marker.user_id IS 'Пользователь, сериализуемый общей tombstone guard.';
COMMENT ON COLUMN recalculation_marker.requested_version IS 'Максимальная ожидаемая calc_version из общей последовательности BSD; отдельно от уже применённой user_data_state.calc_version, чтобы до доставки метрик сохранять recalculating=true.';
COMMENT ON COLUMN recalculation_marker.created_at IS 'Время создания маркера';
ALTER TABLE period_snapshot ADD COLUMN time_zone text;
COMMENT ON COLUMN period_snapshot.time_zone IS 'Исходный IANA-пояс снимка из того же пакета производителя; NULL до новой доставки';
