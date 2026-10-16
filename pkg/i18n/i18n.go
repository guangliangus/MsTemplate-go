// Package i18n is the catalogue of user-facing messages per error code, in every language the
// platform serves. The error writer's Localize customizer puts the caller's language's message on
// the problem title; the errorCode stays the machine-readable contract and the detail stays the
// developer-facing sentence.
//
// The kit ships the platform codes (locales/*.json); a service merges its own codes on top from its
// own embedded files. Language is negotiated from X-Language (the platform's header, one tag) and
// then Accept-Language, through golang.org/x/text's matcher, so zh-Hant-TW finds zh-TW and en-US
// finds en. A guard test keeps every code translated in every shipped language.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"

	"golang.org/x/text/language"
)

//go:embed locales/*.json
var platformLocales embed.FS

// Fallback is the language every catalogue must carry and the one served when nothing matches.
const Fallback = "en"

// Catalog maps locale and error code to a message.
type Catalog struct {
	messages map[string]map[string]string
	locales  []string // Fallback first
	tags     []language.Tag
	matcher  language.Matcher
}

// Platform returns the kit's catalogue of the platform codes; Merge a service's own on top.
func Platform() (*Catalog, error) {
	return Load(platformLocales, "locales")
}

// Load reads <dir>/<locale>.json files from fsys: each a flat object of code -> message, named by
// its BCP 47 tag (en.json, zh-TW.json).
func Load(fsys fs.FS, dir string) (*Catalog, error) {
	c := &Catalog{messages: map[string]map[string]string{}}
	if err := c.Merge(fsys, dir); err != nil {
		return nil, err
	}
	if _, ok := c.messages[Fallback]; !ok {
		return nil, fmt.Errorf("i18n: %s/%s.json is required", dir, Fallback)
	}
	return c, nil
}

// Merge overlays more locale files: a service adds its own codes beside the platform's, an existing
// message is overwritten, a new language is added.
func (c *Catalog) Merge(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("i18n: read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || path.Ext(entry.Name()) != ".json" {
			continue
		}
		locale := strings.TrimSuffix(entry.Name(), ".json")
		if _, err := language.Parse(locale); err != nil {
			return fmt.Errorf("i18n: %s is not a language tag: %w", entry.Name(), err)
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, entry.Name()))
		if err != nil {
			return fmt.Errorf("i18n: read %s: %w", entry.Name(), err)
		}
		var messages map[string]string
		if err := json.Unmarshal(data, &messages); err != nil {
			return fmt.Errorf("i18n: parse %s: %w", entry.Name(), err)
		}
		if c.messages[locale] == nil {
			c.messages[locale] = map[string]string{}
		}
		for code, msg := range messages {
			c.messages[locale][code] = msg
		}
	}
	c.rebuild()
	return nil
}

func (c *Catalog) rebuild() {
	locales := make([]string, 0, len(c.messages))
	for locale := range c.messages {
		if locale != Fallback {
			locales = append(locales, locale)
		}
	}
	sort.Strings(locales)
	if _, ok := c.messages[Fallback]; ok {
		locales = append([]string{Fallback}, locales...)
	}
	tags := make([]language.Tag, len(locales))
	for i, locale := range locales {
		tags[i] = language.Make(locale)
	}
	c.locales, c.tags = locales, tags
	if len(tags) > 0 {
		c.matcher = language.NewMatcher(tags)
	}
}

// Locales lists the catalogue's languages, Fallback first.
func (c *Catalog) Locales() []string {
	return append([]string(nil), c.locales...)
}

// Negotiate picks the catalogue language for a request: X-Language first, then Accept-Language.
func (c *Catalog) Negotiate(r *http.Request) string {
	return c.Match(r.Header.Get("X-Language"), r.Header.Get("Accept-Language"))
}

// Match resolves language preferences, each an Accept-Language value, in order of priority, to one
// catalogue language; nothing usable gives the Fallback. Underscores are tolerated (zh_CN).
func (c *Catalog) Match(prefs ...string) string {
	var tags []language.Tag
	for _, pref := range prefs {
		pref = strings.ReplaceAll(strings.TrimSpace(pref), "_", "-")
		if pref == "" {
			continue
		}
		if parsed, _, err := language.ParseAcceptLanguage(pref); err == nil {
			tags = append(tags, parsed...)
		}
	}
	if len(tags) == 0 || c.matcher == nil {
		return Fallback
	}
	_, index, confidence := c.matcher.Match(tags...)
	if confidence == language.No || index < 0 || index >= len(c.locales) {
		return Fallback
	}
	return c.locales[index]
}

// Message returns the message for code in locale, or in the Fallback language when the locale has
// none; false when neither has it.
func (c *Catalog) Message(code, locale string) (string, bool) {
	if msg, ok := c.messages[locale][code]; ok {
		return msg, true
	}
	msg, ok := c.messages[Fallback][code]
	return msg, ok
}

// Translate implements httpx.Translator.
func (c *Catalog) Translate(r *http.Request, code string) (string, bool) {
	return c.Message(code, c.Negotiate(r))
}

// Missing reports which of the codes have no message in locale, for the guard that keeps the
// catalogue complete.
func (c *Catalog) Missing(locale string, codes []string) []string {
	var missing []string
	for _, code := range codes {
		if _, ok := c.messages[locale][code]; !ok {
			missing = append(missing, code)
		}
	}
	return missing
}

// Messages returns a copy of one locale's table, for guards that budget message length.
func (c *Catalog) Messages(locale string) map[string]string {
	out := make(map[string]string, len(c.messages[locale]))
	for code, msg := range c.messages[locale] {
		out[code] = msg
	}
	return out
}
