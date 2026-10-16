package reqmeta

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestCaptureKeepsKnownCleanHeadersOnly(t *testing.T) {
	h := http.Header{}
	h.Set("X-Request-ID", " req-1 ")
	h.Set("X-Language", "zh-TW")
	h.Set("X-Device-ID", "bad\nvalue")
	h.Set("X-App-Version", strings.Repeat("9", MaxValueLength+1))
	h.Set("Authorization", "Bearer secret")

	ctx := Capture(context.Background(), h)
	if got := RequestID(ctx); got != "req-1" {
		t.Fatalf("request id %q", got)
	}
	if got := Value(ctx, "x-language"); got != "zh-TW" {
		t.Fatalf("lookup is case-insensitive: %q", got)
	}
	for _, name := range []string{HeaderDeviceID, HeaderAppVersion, "Authorization"} {
		if got := Value(ctx, name); got != "" {
			t.Fatalf("%s must not be captured: %q", name, got)
		}
	}
	if Value(context.Background(), HeaderRequestID) != "" {
		t.Fatal("an untouched context carries nothing")
	}
}
