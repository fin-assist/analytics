package metrics

import (
	"errors"
	"strings"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
)

func (p composition) unavailable(id string) *commonv1.Block {
	b := unavailableBlock(id)
	if id == "tiles" && p.tileVersion == 2 {
		b.V = 2
		b.Data, _ = data(map[string]any{"groups": []any{}})
	}
	return b
}

// groupTiles changes only layout metadata. Version 1 retains its exact payload and order.
func (p composition) groupTiles(b *commonv1.Block) error {
	if p.tileVersion != 2 {
		return nil
	}
	b.V = 2
	if b.Status == "none" || b.Status == "unavailable" {
		var err error
		b.Data, err = data(map[string]any{"groups": []any{}})
		return err
	}
	flat, ok := b.GetData().AsMap()["tiles"].([]any)
	if !ok {
		return errors.New("tile group payload absent")
	}
	byID := map[string]map[string]any{}
	for _, raw := range flat {
		tile, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid tile payload")
		}
		id, ok := tile["id"].(string)
		if !ok {
			return errors.New("tile id absent")
		}
		byID[id] = tile
	}
	for _, id := range []string{"expense", "income", "balance", "daily_expense"} {
		if byID[id] == nil {
			return errors.New("required fact tile absent")
		}
	}
	byID["expense"]["title"] = "Расходы за период"
	byID["income"]["title"] = "Доходы за период"
	daily := byID["daily_expense"]
	daily["title"] = "Средние расходы в день"
	if daily["status"] == "ready" || daily["status"] == "tentative" {
		caption := "По дням с данными не позже сегодня; пропуски не учитываются"
		if previous, ok := daily["caption"].(string); ok && strings.TrimSpace(previous) != "" {
			caption += "\n" + previous
		}
		daily["caption"] = caption
	}
	facts := []any{byID["expense"], byID["income"], byID["balance"]}
	additional := []any{daily}
	if forecast := byID["forecast"]; forecast != nil {
		additional = append(additional, forecast)
	}
	var err error
	b.Data, err = data(map[string]any{"groups": []any{
		map[string]any{"tiles": facts},
		map[string]any{"title": "Дополнительные показатели", "tiles": additional},
	}})
	if err != nil {
		return err
	}
	parts := []string{}
	for _, raw := range append(facts, additional...) {
		tile := raw.(map[string]any)
		part := tile["title"].(string)
		if value, ok := tile["value"].(float64); ok {
			part += " " + money(int64(value))
		} else if lock, ok := tile["lock"].(map[string]any); ok {
			part += " — " + lock["text"].(string)
		} else {
			part += " — пока недоступно"
		}
		parts = append(parts, part)
	}
	b.AltText = ptr(strings.Join(parts, ", "))
	return nil
}
