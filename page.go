package gormx

import "fmt"

const (
	DefaultPageSize = 20
	MaxPageSize     = 500
)

// PageRequest is one-based. Zero values select page 1 with DefaultPageSize.
type PageRequest struct {
	Current int `json:"current"`
	Size    int `json:"size"`
}

type Page[T any] struct {
	Records []T   `json:"records"`
	Total   int64 `json:"total"`
	Current int   `json:"current"`
	Size    int   `json:"size"`
	Pages   int64 `json:"pages"`
}

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
