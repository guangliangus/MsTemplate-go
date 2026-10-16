package money

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

func TestAmountTravelsAsAJSONNumber(t *testing.T) {
	body, err := json.Marshal(struct {
		Total Amount `json:"total"`
	}{From(decimal.RequireFromString("39.98"))})
	if err != nil || string(body) != `{"total":39.98}` {
		t.Fatalf("got %s %v", body, err)
	}
	if decimal.MarshalJSONWithoutQuotes {
		t.Fatal("the format must not depend on the shopspring global")
	}
}

func TestAmountReadsNumbersAndNumericStrings(t *testing.T) {
	for _, raw := range []string{`{"total":19.99}`, `{"total":"19.99"}`} {
		var v struct {
			Total Amount `json:"total"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil || !v.Total.Decimal().Equal(decimal.RequireFromString("19.99")) {
			t.Fatalf("%s: %v %v", raw, v.Total, err)
		}
	}
	var v struct {
		Total Amount `json:"total"`
	}
	if err := json.Unmarshal([]byte(`{"total":"abc"}`), &v); err == nil {
		t.Fatal("not a number")
	}
}
