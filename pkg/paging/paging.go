// Package paging is the uniform pagination contract for list endpoints, in two shapes:
//
//   - numbered pages, ?page=&pageSize= in and a Result envelope with totals out, for lists that
//     stay small (one customer's orders): the total is a count(*) and page N is an OFFSET, both of
//     which cost more the further the list goes;
//   - keyset pages, ?cursor=&limit= in and a CursorResult out, for lists that grow without bound
//     (every order, an event log): each page is an index range scan that costs the same on the
//     first page and the ten-thousandth, and nothing is counted.
//
// These envelopes are the only wrapping the API layer does around a response type.
package paging

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"

	"example.com/msvcservice/pkg/apperror"
)

const (
	// DefaultPageSize applies when the query carries no pageSize (or limit).
	DefaultPageSize = 20
	// MaxPageSize caps pageSize and limit; larger values are clamped, not refused.
	MaxPageSize = 200
	// MaxPage bounds numbered paging: past it a list belongs on keyset paging. It also keeps the
	// offset arithmetic far from overflowing into a negative OFFSET.
	MaxPage = 10_000
)

// Request is the normalised page request.
type Request struct {
	Page     int
	PageSize int
}

// Parse reads page and pageSize from the query string and normalises them: a page below 1 is 1,
// a page size below 1 is the default and one above MaxPageSize is clamped. A page past MaxPage is
// a 400 VALIDATION_FAILED: silently serving another page would be wrong, and an empty one would
// hide that the client should be using a cursor.
func Parse(query url.Values) (Request, error) {
	page, _ := strconv.Atoi(query.Get("page"))
	size, _ := strconv.Atoi(query.Get("pageSize"))
	if page > MaxPage {
		return Request{}, apperror.Validation(map[string][]string{
			"page": {"must be at most " + strconv.Itoa(MaxPage)},
		})
	}
	return Request{Page: page, PageSize: size}.Normalized(), nil
}

// Normalized returns a copy with the page and size brought into range.
func (r Request) Normalized() Request {
	r.Page = min(max(r.Page, 1), MaxPage)
	r.PageSize = normalizeSize(r.PageSize)
	return r
}

func normalizeSize(size int) int {
	if size < 1 {
		return DefaultPageSize
	}
	return min(size, MaxPageSize)
}

// Offset is the number of rows to skip for this page.
func (r Request) Offset() int {
	n := r.Normalized()
	return (n.Page - 1) * n.PageSize
}

// Limit is the number of rows to take.
func (r Request) Limit() int { return r.Normalized().PageSize }

// Result is the page envelope.
type Result[T any] struct {
	Items       []T   `json:"items"`
	Page        int   `json:"page"`
	PageSize    int   `json:"pageSize"`
	TotalCount  int64 `json:"totalCount"`
	TotalPages  int64 `json:"totalPages"`
	HasNextPage bool  `json:"hasNextPage"`
}

// NewResult builds the envelope. Items is never null on the wire.
func NewResult[T any](items []T, req Request, total int64) Result[T] {
	n := req.Normalized()
	if items == nil {
		items = []T{}
	}
	pages := (total + int64(n.PageSize) - 1) / int64(n.PageSize)
	return Result[T]{
		Items:       items,
		Page:        n.Page,
		PageSize:    n.PageSize,
		TotalCount:  total,
		TotalPages:  pages,
		HasNextPage: int64(n.Page) < pages,
	}
}

// cursorPrefix versions the opaque cursor, so its encoding can change without misreading old ones.
const cursorPrefix = "v1:"

// CursorRequest is a keyset page request: the rows after After (0 = from the start) in the list's
// own order, at most Limit of them. The repository reads Fetch rows — one more than Limit — so
// NewCursorResult can tell whether another page exists without counting anything.
type CursorRequest struct {
	After int64
	Limit int
}

// ParseCursor reads cursor and limit from the query string. The cursor is opaque to clients: they
// pass back the nextCursor of the previous page. A cursor that does not decode is a 400.
func ParseCursor(query url.Values) (CursorRequest, error) {
	limit, _ := strconv.Atoi(query.Get("limit"))
	req := CursorRequest{Limit: normalizeSize(limit)}
	raw := query.Get("cursor")
	if raw == "" {
		return req, nil
	}
	after, ok := decodeCursor(raw)
	if !ok {
		return CursorRequest{}, apperror.Validation(map[string][]string{
			"cursor": {"is not a cursor this API issued"},
		})
	}
	req.After = after
	return req, nil
}

// Fetch is how many rows the repository reads: Limit plus one, the probe for a next page.
func (r CursorRequest) Fetch() int { return normalizeSize(r.Limit) + 1 }

// EncodeCursor makes the opaque cursor for a key.
func EncodeCursor(key int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.FormatInt(key, 10)))
}

func decodeCursor(raw string) (int64, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, false
	}
	digits, ok := strings.CutPrefix(string(b), cursorPrefix)
	if !ok {
		return 0, false
	}
	key, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || key < 1 {
		return 0, false
	}
	return key, true
}

// CursorResult is the keyset page envelope. NextCursor is set only when HasMore.
type CursorResult[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
	HasMore    bool   `json:"hasMore"`
}

// NewCursorResult builds the envelope from the rows the repository read for req (up to req.Fetch());
// key returns the keyset key of a row, the value the next page continues after.
func NewCursorResult[T any](rows []T, req CursorRequest, key func(T) int64) CursorResult[T] {
	limit := normalizeSize(req.Limit)
	res := CursorResult[T]{Items: rows}
	if len(rows) > limit {
		res.Items = rows[:limit]
		res.HasMore = true
		res.NextCursor = EncodeCursor(key(res.Items[limit-1]))
	}
	if res.Items == nil {
		res.Items = []T{}
	}
	return res
}
