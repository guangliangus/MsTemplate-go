package paging

import (
	"net/url"
	"testing"

	"example.com/msvcservice/pkg/apperror"
)

func TestParseNormalises(t *testing.T) {
	cases := map[string]Request{
		"":                      {Page: 1, PageSize: DefaultPageSize},
		"page=0&pageSize=0":     {Page: 1, PageSize: DefaultPageSize},
		"page=3&pageSize=50":    {Page: 3, PageSize: 50},
		"page=2&pageSize=99999": {Page: 2, PageSize: MaxPageSize},
		"page=abc":              {Page: 1, PageSize: DefaultPageSize},
		"page=10000":            {Page: MaxPage, PageSize: DefaultPageSize},
	}
	for raw, want := range cases {
		q, _ := url.ParseQuery(raw)
		got, err := Parse(q)
		if err != nil || got != want {
			t.Errorf("%q: got %+v %v, want %+v", raw, got, err, want)
		}
	}
}

func TestParseRefusesPagesPastTheLimit(t *testing.T) {
	// MaxInt64 used to overflow (page-1)*pageSize into a negative OFFSET: a 500 from the database.
	for _, raw := range []string{"page=10001", "page=9223372036854775807"} {
		q, _ := url.ParseQuery(raw)
		_, err := Parse(q)
		if e, ok := apperror.As(err); !ok || e.Status != 400 || len(e.Fields["page"]) == 0 {
			t.Errorf("%q: %v", raw, err)
		}
	}
	if off := (Request{Page: 1 << 62, PageSize: 200}).Offset(); off < 0 || off > MaxPage*MaxPageSize {
		t.Fatalf("a hand-built request is clamped too: offset %d", off)
	}
}

func TestOffsetAndLimit(t *testing.T) {
	r := Request{Page: 3, PageSize: 20}
	if r.Offset() != 40 || r.Limit() != 20 {
		t.Fatalf("offset %d limit %d", r.Offset(), r.Limit())
	}
}

func TestNewResult(t *testing.T) {
	res := NewResult([]int{1, 2}, Request{Page: 2, PageSize: 2}, 5)
	if res.TotalPages != 3 || !res.HasNextPage || res.Page != 2 || res.PageSize != 2 {
		t.Fatalf("unexpected envelope %+v", res)
	}
	last := NewResult([]int{5}, Request{Page: 3, PageSize: 2}, 5)
	if last.HasNextPage {
		t.Fatal("the last page has no next page")
	}
	empty := NewResult[int](nil, Request{}, 0)
	if empty.Items == nil || empty.TotalPages != 0 {
		t.Fatal("items must never be null on the wire")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	q := url.Values{"limit": {"2"}}
	first, err := ParseCursor(q)
	if err != nil || first.After != 0 || first.Limit != 2 || first.Fetch() != 3 {
		t.Fatalf("first page: %+v %v", first, err)
	}
	// Rows newest first, as the repository returns them; three read for a limit of two.
	id := func(n int64) int64 { return n }
	page := NewCursorResult([]int64{30, 20, 10}, first, id)
	if len(page.Items) != 2 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("first page: %+v", page)
	}
	next, err := ParseCursor(url.Values{"cursor": {page.NextCursor}, "limit": {"2"}})
	if err != nil || next.After != 20 {
		t.Fatalf("the cursor continues after the last row: %+v %v", next, err)
	}
	last := NewCursorResult([]int64{10}, next, id)
	if last.HasMore || last.NextCursor != "" || len(last.Items) != 1 {
		t.Fatalf("last page: %+v", last)
	}
	if empty := NewCursorResult[int64](nil, next, id); empty.Items == nil {
		t.Fatal("items must never be null on the wire")
	}
}

func TestParseCursorRefusesForgeries(t *testing.T) {
	for _, raw := range []string{"!!", "MTIz", EncodeCursor(0)} { // not base64, no version, not a key
		if _, err := ParseCursor(url.Values{"cursor": {raw}}); apperror.StatusOf(err) != 400 {
			t.Errorf("%q: %v", raw, err)
		}
	}
}
