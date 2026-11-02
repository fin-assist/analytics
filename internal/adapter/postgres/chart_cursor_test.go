package postgres

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/fin-assist/analytics/internal/domain"
)

func TestUnitChartCursorValidatesOwnerFormatAndPositionBeforeDatabase(t *testing.T) {
	user, other := newUser(416), newUser(417)
	revision := strings.Repeat("a", 64)
	good := encodeChartCursor(user, domain.TransferWithout, revision, 0)
	value, err := decodeChartCursor(user, good)
	if err != nil || value.Mode != domain.TransferWithout || value.Revision != revision || value.Page != 0 {
		t.Fatal(value, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(good)
	if err != nil {
		t.Fatal(err)
	}
	altered := func(old, replacement string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), old, replacement, 1)))
	}
	for name, cursor := range map[string]string{
		"unsupported version": altered(`"v":1`, `"v":2`),
		"unsupported mode":    altered(`"mode":2`, `"mode":3`),
		"negative page":       altered(`"page":0`, `"page":-1`),
		"fractional page":     altered(`"page":0`, `"page":0.5`),
		"overflow page":       altered(`"page":0`, `"page":18446744073709551616`),
		"missing page":        altered(`,"page":0`, ``),
		"duplicate page":      altered(`"page":0`, `"page":0,"page":1`),
		"invalid revision":    altered(revision, strings.Repeat("z", 64)),
		"oversize":            strings.Repeat("a", 2049),
		"trailing value":      base64.RawURLEncoding.EncodeToString(append(raw, []byte(`{}`)...)),
		"foreign stale":       encodeChartCursor(other, domain.TransferWith, strings.Repeat("f", 64), 0),
	} {
		t.Run(name, func(t *testing.T) {
			repo := &Metrics{} // Invalid/foreign data is rejected before a database can be selected.
			if _, err := repo.ReadChartPage(t.Context(), user, cursor); !errors.Is(err, domain.ErrCursorFormat) {
				t.Fatal(err)
			}
		})
	}
}

func FuzzUnitChartCursorCannotSelectAnotherOwner(f *testing.F) {
	user := newUser(416)
	f.Add(encodeChartCursor(user, domain.TransferWith, strings.Repeat("a", 64), 1))
	f.Add(encodeChartCursor(newUser(417), domain.TransferWith, strings.Repeat("a", 64), 1))
	f.Add("")
	f.Fuzz(func(t *testing.T, cursor string) {
		value, err := decodeChartCursor(user, cursor)
		if err != nil {
			return
		}
		if value.Owner != chartOwner(user) || value.Page < 0 || (value.Mode != domain.TransferWith && value.Mode != domain.TransferWithout) || len(cursor) > 2048 {
			t.Fatal(value)
		}
		if _, err = decodeChartCursor(newUser(417), cursor); !errors.Is(err, domain.ErrCursorFormat) {
			t.Fatal("foreign owner accepted")
		}
	})
}
