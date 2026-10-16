package telemetry

import (
	"strings"
	"testing"
)

func TestBoundedLabel(t *testing.T) {
	cases := map[string]string{
		"NOT_FOUND":                           "NOT_FOUND",
		"ORDER_ITEM_UNAVAILABLE":              "ORDER_ITEM_UNAVAILABLE",
		"":                                    "OTHER",
		"not_found":                           "OTHER",
		"has space":                           "OTHER",
		"id=42":                               "OTHER",
		strings.Repeat("A", MaxLabelLength+1): "OTHER",
	}
	for in, want := range cases {
		if got := BoundedLabel(in, "OTHER"); got != want {
			t.Errorf("BoundedLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
