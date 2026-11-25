package domain

import (
	"context"
	"errors"
)

// fetchPage reads one unfiltered page of at most size items after token.
type fetchPage[T any] func(ctx context.Context, size int, token string) (items []T, next string, err error)

// visiblePage returns one page of up to size items that visible accepts,
// resuming after token, and the token of the next page ("" when no visible
// item is left).
//
// Filtering never breaks paging: a page is filled from as many underlying
// pages as needed, and its token resumes exactly after the last item it
// returned. Each underlying read asks for at most the number of slots still
// free, so a filled page always ends on an underlying page boundary and the
// underlying next token is a correct resume point. Before handing out a token
// it looks ahead for one more visible item, so the last page is never empty.
func visiblePage[T any](ctx context.Context, size int, token string, fetch fetchPage[T], visible func(T) (bool, error)) (page []T, nextToken string, err error) {
	if size <= 0 {
		return nil, "", errors.New("visiblePage: size must be positive")
	}
	page = make([]T, 0, size)
	cur := token
	for len(page) < size {
		items, next, ferr := fetch(ctx, size-len(page), cur)
		if ferr != nil {
			return nil, "", ferr
		}
		for _, it := range items {
			ok, verr := visible(it)
			if verr != nil {
				return nil, "", verr
			}
			if ok {
				page = append(page, it)
			}
		}
		if next == "" || len(items) == 0 {
			return page, "", nil
		}
		cur = next
	}
	more, err := anyVisible(ctx, size, cur, fetch, visible)
	if err != nil || !more {
		return page, "", err
	}
	return page, cur, nil
}

// anyVisible reports whether a visible item exists after token.
func anyVisible[T any](ctx context.Context, size int, token string, fetch fetchPage[T], visible func(T) (bool, error)) (bool, error) {
	for {
		items, next, err := fetch(ctx, size, token)
		if err != nil {
			return false, err
		}
		for _, it := range items {
			ok, err := visible(it)
			if err != nil || ok {
				return ok, err
			}
		}
		if next == "" || len(items) == 0 {
			return false, nil
		}
		token = next
	}
}
