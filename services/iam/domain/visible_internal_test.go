package domain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
)

// sliceFetch pages over items with index-based tokens.
func sliceFetch(items []int) fetchPage[int] {
	return func(_ context.Context, size int, token string) ([]int, string, error) {
		start := 0
		if token != "" {
			n, err := strconv.Atoi(token)
			if err != nil {
				return nil, "", err
			}
			start = n
		}
		end := min(start+size, len(items))
		next := ""
		if end < len(items) {
			next = strconv.Itoa(end)
		}
		return slices.Clone(items[start:end]), next, nil
	}
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func maskVisible(mask []bool) func(int) (bool, error) {
	return func(i int) (bool, error) { return mask[i], nil }
}

// collect follows tokens and returns the pages.
func collect(t *testing.T, items []int, size int, vis func(int) (bool, error)) [][]int {
	t.Helper()
	var pages [][]int
	fetch := sliceFetch(items)
	token := ""
	for range 4 * (len(items) + 2) {
		page, next, err := visiblePage(t.Context(), size, token, fetch, vis)
		if err != nil {
			t.Fatalf("visiblePage: %v", err)
		}
		pages = append(pages, page)
		if next == "" {
			return pages
		}
		token = next
	}
	t.Fatal("paging did not terminate")
	return nil
}

func flatten(pages [][]int) []int {
	var out []int
	for _, p := range pages {
		out = append(out, p...)
	}
	return out
}

func TestVisiblePageTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		n         int
		visible   []int
		size      int
		wantPages [][]int
	}{
		{"no filtering size 1", 3, []int{0, 1, 2}, 1, [][]int{{0}, {1}, {2}}},
		{"no filtering large size", 3, []int{0, 1, 2}, 10, [][]int{{0, 1, 2}}},
		{"empty source", 0, nil, 3, [][]int{{}}},
		{"all invisible", 5, nil, 2, [][]int{{}}},
		{"invisible tail after full page", 5, []int{0, 1}, 2, [][]int{{0, 1}}},
		{"invisible head", 5, []int{3, 4}, 1, [][]int{{3}, {4}}},
		{"long invisible runs between", 12, []int{0, 6, 11}, 1, [][]int{{0}, {6}, {11}}},
		{"long invisible runs size 2", 12, []int{0, 6, 11}, 2, [][]int{{0, 6}, {11}}},
		{"exact multiple no extra page", 4, []int{0, 1, 2, 3}, 2, [][]int{{0, 1}, {2, 3}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mask := make([]bool, tt.n)
			for _, v := range tt.visible {
				mask[v] = true
			}
			got := collect(t, seq(tt.n), tt.size, maskVisible(mask))
			if len(got) != len(tt.wantPages) {
				t.Fatalf("pages = %v, want %v", got, tt.wantPages)
			}
			for i := range got {
				if !slices.Equal(got[i], tt.wantPages[i]) {
					t.Errorf("page %d = %v, want %v", i, got[i], tt.wantPages[i])
				}
			}
		})
	}
}

func TestVisiblePageErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	okFetch := sliceFetch(seq(3))
	alwaysVisible := func(int) (bool, error) { return true, nil }

	t.Run("size not positive", func(t *testing.T) {
		t.Parallel()
		for _, size := range []int{0, -1} {
			if _, _, err := visiblePage(t.Context(), size, "", okFetch, alwaysVisible); err == nil {
				t.Errorf("size %d: want error", size)
			}
		}
	})
	t.Run("visible error", func(t *testing.T) {
		t.Parallel()
		_, _, err := visiblePage(t.Context(), 2, "", okFetch, func(int) (bool, error) { return false, boom })
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
	t.Run("visible error in lookahead", func(t *testing.T) {
		t.Parallel()
		_, _, err := visiblePage(t.Context(), 1, "", okFetch, func(i int) (bool, error) {
			if i == 1 {
				return false, boom
			}
			return true, nil
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
	t.Run("fetch error", func(t *testing.T) {
		t.Parallel()
		fetch := func(context.Context, int, string) ([]int, string, error) { return nil, "", boom }
		if _, _, err := visiblePage(t.Context(), 2, "", fetch, alwaysVisible); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
	t.Run("fetch error in lookahead", func(t *testing.T) {
		t.Parallel()
		inner := sliceFetch(seq(5))
		n := 0
		fetch := func(ctx context.Context, size int, tok string) ([]int, string, error) {
			n++
			if n > 1 {
				return nil, "", boom
			}
			return inner(ctx, size, tok)
		}
		if _, _, err := visiblePage(t.Context(), 2, "", fetch, alwaysVisible); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}

// TestVisiblePageProperty checks, for every size and every visibility mask of
// a 9-item list, that the pages concatenate to the filtered list, no page
// exceeds size, and no page is empty unless the whole result is.
func TestVisiblePageProperty(t *testing.T) {
	t.Parallel()
	const n = 9
	items := seq(n)
	for size := 1; size <= 7; size++ {
		t.Run(fmt.Sprintf("size %d", size), func(t *testing.T) {
			t.Parallel()
			for bits := range 1 << n {
				mask := make([]bool, n)
				var want []int
				for i := range n {
					mask[i] = bits&(1<<i) != 0
					if mask[i] {
						want = append(want, i)
					}
				}
				pages := collect(t, items, size, maskVisible(mask))
				if got := flatten(pages); !slices.Equal(got, want) {
					t.Fatalf("mask %09b size %d: got %v, want %v", bits, size, got, want)
				}
				for i, p := range pages {
					if len(p) > size {
						t.Fatalf("mask %09b: page %d has %d items > size %d", bits, i, len(p), size)
					}
					if len(p) == 0 && len(want) != 0 {
						t.Fatalf("mask %09b size %d: empty page %d in %v", bits, size, i, pages)
					}
				}
			}
		})
	}
}
