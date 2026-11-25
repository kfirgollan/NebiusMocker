package domain

import (
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// pageSize validates a requested page size and applies the default.
func pageSize(requested int64) (int, error) {
	if requested < 0 || requested > maxPageSize {
		return 0, errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize),
		})
	}
	if requested == 0 {
		return defaultPageSize, nil
	}
	return int(requested), nil
}

func encodeOffset(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(n)))
}

func decodeOffset(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err == nil && len(b) > 1 && b[0] == 'o' {
		if n, perr := strconv.Atoi(string(b[1:])); perr == nil && n >= 0 {
			return n, nil
		}
	}
	return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
}
