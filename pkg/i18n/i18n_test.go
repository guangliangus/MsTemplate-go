package i18n

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"example.com/msvcservice/pkg/apperror"
)

// Hard caps per language, in characters: the copy goes to a single-line toast on the apps.
var budgets = map[string]int{"en": 40, "ja": 40, "zh-CN": 16, "zh-TW": 16}

func TestPlatformCatalogueCoversEveryCodeInEveryLanguageWithinBudget(t *testing.T) {
	c, err := Platform()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Locales(); got[0] != Fallback || len(got) != 4 {
		t.Fatalf("locales %v", got)
	}
	for _, locale := range c.Locales() {
		if missing := c.Missing(locale, apperror.Codes()); len(missing) > 0 {
			t.Errorf("%s.json lacks %v", locale, missing)
		}
		limit, ok := budgets[locale]
		if !ok {
			t.Fatalf("no length budget for %s", locale)
		}
		for code, msg := range c.Messages(locale) {
			if n := utf8.RuneCountInString(msg); n > limit || strings.TrimSpace(msg) == "" {
				t.Errorf("%s/%s: %d characters, budget %d: %q", locale, code, n, limit, msg)
			}
		}
	}
}

func TestNegotiationAndFallbacks(t *testing.T) {
	c, err := Platform()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ xLanguage, accept, want string }{
		{"zh-TW", "", "zh-TW"},
		{"zh-Hant-TW", "", "zh-TW"},
		{"zh_CN", "", "zh-CN"},
		{"zh", "", "zh-CN"},
		{"en-US", "ja", "en"},
		{"", "ja, en;q=0.8", "ja"},
		{"", "fr-FR, de;q=0.9", "en"},
		{"garbage!!", "", "en"},
		{"", "", "en"},
	}
	for _, tc := range cases {
		if got := c.Match(tc.xLanguage, tc.accept); got != tc.want {
			t.Errorf("X-Language=%q Accept-Language=%q: got %s want %s", tc.xLanguage, tc.accept, got, tc.want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Language", "ja")
	if msg, ok := c.Translate(req, "NOT_FOUND"); !ok || msg != "見つかりません" {
		t.Fatalf("translate: %q %v", msg, ok)
	}
	if _, ok := c.Translate(req, "NO_SUCH_CODE"); ok {
		t.Fatal("unknown codes are not translated")
	}
}

func TestMergeAddsServiceCodesAndLanguages(t *testing.T) {
	c, err := Platform()
	if err != nil {
		t.Fatal(err)
	}
	overlay := fstest.MapFS{
		"locales/en.json": {Data: []byte(`{"ORDER_FULL": "The order is full.", "NOT_FOUND": "Nothing here."}`)},
		"locales/ko.json": {Data: []byte(`{"ORDER_FULL": "주문이 가득 찼습니다"}`)},
	}
	if err := c.Merge(overlay, "locales"); err != nil {
		t.Fatal(err)
	}
	if msg, _ := c.Message("ORDER_FULL", "zh-TW"); msg != "The order is full." {
		t.Fatalf("a language without the code falls back to English: %q", msg)
	}
	if msg, _ := c.Message("NOT_FOUND", "en"); msg != "Nothing here." {
		t.Fatalf("overlay overrides: %q", msg)
	}
	if got := c.Match("ko-KR"); got != "ko" {
		t.Fatalf("new language joins negotiation: %s", got)
	}
	bad := fstest.MapFS{"locales/not a tag.json": {Data: []byte(`{}`)}}
	if err := c.Merge(bad, "locales"); err == nil {
		t.Fatal("a file that is not a language tag is refused")
	}
}
