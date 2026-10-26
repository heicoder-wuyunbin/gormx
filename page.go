package gormx

import "fmt"

const (
	// DefaultPageSize is the page size applied when PageRequest.Size is zero.
	// It is resolved once per query by PageRequest.normalize.
	DefaultPageSize = 20

	// MaxPageSize is the hard upper bound for PageRequest.Size.
	// Larger values are rejected with ErrInvalidPage rather than silently
	// clamped, so a bad request cannot accidentally scan an unbounded
	// number of rows.
	MaxPageSize = 500
)

// PageRequest selects one page of rows for SelectPage and JoinPage.
//
// Pages are one-based: Current = 1 means the first page. The zero value
// {Current: 0, Size: 0} is valid and means page 1 with DefaultPageSize rows.
//
// PageRequest is a plain data carrier and does not validate on construction.
// Validation and defaulting happen when a query executes, so an invalid
// request only fails at SelectPage/JoinPage with an error wrapping
// ErrInvalidPage:
//
//   - Current < 0 or Size < 0 is rejected. Negative values are never treated
//     as "use the default".
//   - Size > MaxPageSize is rejected.
//   - (Current-1)*Size overflowing int is rejected.
//
// A page beyond the last page is legal and yields empty records, not an
// error. JSON tags allow direct binding from request payloads, but the
// parsing layer should still reject non-numeric input; validation here is
// the last line of defense, not the primary input check.
type PageRequest struct {
	// Current is the one-based page number. Zero selects page 1.
	Current int `json:"current"`
	// Size is the number of rows per page. Zero selects DefaultPageSize;
	// values above MaxPageSize are rejected with ErrInvalidPage.
	Size int `json:"size"`
}

// Page is the result envelope returned by SelectPage and JoinPage.
//
// It carries one page of records plus the counters needed to render a pager.
// Current and Size echo the values actually used after normalization, not
// necessarily the raw request. Total and Pages are exact for the moment the
// COUNT statement ran.
//
// The meaning of Total depends on the producer:
//
//   - SelectPage: counts root model rows of type T.
//   - JoinPage: counts JOIN result rows of type R. A one-to-many join can
//     therefore report the same root entity several times, and it must not be
//     used to paginate "parent + children" collections.
//
// On success Records is always non-nil and serializes as [] rather than null
// when nothing matches. Row order is unspecified unless an explicit Order is
// supplied; producers append primary keys as tie-breakers so offset
// pagination stays stable across pages.
//
// Page is assembled from two independent statements (COUNT and SELECT), so
// under concurrent writes Total and Records may reflect slightly different
// moments, and inserted or deleted rows can shift OFFSET pages. Wrap both
// statements in a repeatable-read transaction when a consistent snapshot is
// required. A Page value is not safe for concurrent mutation.
type Page[T any] struct {
	// Records holds the rows of this page. It is a non-nil slice after a
	// successful query, empty when the page is out of range or matches
	// nothing.
	Records []T `json:"records"`
	// Total is the exact number of rows matching the query: root rows for
	// SelectPage, joined rows for JoinPage.
	Total int64 `json:"total"`
	// Current is the normalized one-based page number that produced this
	// result.
	Current int `json:"current"`
	// Size is the normalized page size used for this result.
	Size int `json:"size"`
	// Pages is the total number of pages, ceil(Total/Size); zero when Total
	// is zero.
	Pages int64 `json:"pages"`
}

// normalize validates the request, applies defaults and returns the effective
// request plus the row offset for OFFSET.
//
// It is the single place where pagination defaults are resolved. Every error
// wraps ErrInvalidPage so callers can match it with errors.Is. Checks run in
// this order:
//
//  1. Reject negative Current/Size and Size > MaxPageSize.
//  2. Replace Current == 0 with 1 and Size == 0 with DefaultPageSize.
//  3. Reject (Current-1)*Size when the multiplication would overflow int.
//
// Validation deliberately precedes defaulting, so a negative value is never
// silently coerced to the first page. The returned request always has
// Current >= 1 and Size in [1, MaxPageSize], and offset equals
// (Current-1)*Size. A request past the last page is not an error; it produces
// an offset beyond the result set and therefore empty records.
//
// normalize only does arithmetic and never touches the database. Callers must
// not mutate the returned request afterwards; it is meant to be used for the
// immediately following COUNT and SELECT pair.
func (r PageRequest) normalize() (PageRequest, int, error) {
	if r.Current < 0 || r.Size < 0 || r.Size > MaxPageSize {
		return r, 0, fmt.Errorf("%w: current >= 0, size must be between 0 and %d", ErrInvalidPage, MaxPageSize)
	}
	if r.Current == 0 {
		r.Current = 1
	}
	if r.Size == 0 {
		r.Size = DefaultPageSize
	}
	maxInt := int(^uint(0) >> 1)
	if r.Current-1 > maxInt/r.Size {
		return r, 0, fmt.Errorf("%w: offset overflows int", ErrInvalidPage)
	}
	return r, (r.Current - 1) * r.Size, nil
}
