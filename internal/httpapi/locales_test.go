package httpapi

import (
	"testing"

	"example.com/msvcservice/internal/order"
)

// Every code this service publishes has a message in every language the catalogue ships.
func TestServiceCodesAreTranslatedEverywhere(t *testing.T) {
	c, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, locale := range c.Locales() {
		if missing := c.Missing(locale, order.Codes()); len(missing) > 0 {
			t.Errorf("locales/%s.json lacks %v", locale, missing)
		}
	}
}
