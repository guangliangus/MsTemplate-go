package telemetry

// MaxLabelLength bounds a free-form metric label value.
const MaxLabelLength = 64

// BoundedLabel keeps a free-form value usable as a metric label: UPPER_SNAKE_CASE (letters, digits,
// underscore) and at most MaxLabelLength characters; anything else, the empty string included,
// becomes fallback. A code echoed back by a provider or a client must never create an unbounded
// set of series, and a shape check is cheaper to keep than an allowlist of every legitimate code.
func BoundedLabel(value, fallback string) string {
	if value == "" || len(value) > MaxLabelLength {
		return fallback
	}
	for i := range len(value) {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return fallback
	}
	return value
}
