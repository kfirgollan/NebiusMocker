package domain

import (
	"net/netip"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// ReservedPrefix is kept for NebiusMocker's internal buckets (for example
// compute image blobs) that share the object store with user buckets.
const ReservedPrefix = "nebius-mocker-"

// ValidateBucketName applies the S3 bucket naming rules: 3–63 characters of
// lower-case letters, digits, '.' and '-', starting and ending with a letter
// or digit, no "..", ".-" or "-.", and not formatted as an IP address.
func ValidateBucketName(name string) error {
	bad := func(msg string) error {
		return errs.BadRequest(errs.Violation{Field: "metadata.name", Message: msg})
	}
	if len(name) < 3 || len(name) > 63 {
		return bad("bucket names must be 3 to 63 characters long")
	}
	for i := range len(name) {
		c := name[i]
		if !isAlnum(c) && c != '.' && c != '-' {
			return bad("bucket names may contain only lower-case letters, digits, '.' and '-'")
		}
	}
	if !isAlnum(name[0]) || !isAlnum(name[len(name)-1]) {
		return bad("bucket names must start and end with a letter or digit")
	}
	for _, seq := range []string{"..", ".-", "-."} {
		if strings.Contains(name, seq) {
			return bad("bucket names must not contain " + seq)
		}
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return bad("bucket names must not be formatted as an IP address")
	}
	if strings.HasPrefix(name, "xn--") || strings.HasSuffix(name, "-s3alias") {
		return bad("bucket names must not start with xn-- or end with -s3alias")
	}
	if strings.HasPrefix(name, ReservedPrefix) {
		return bad("the prefix " + ReservedPrefix + " is reserved for NebiusMocker's internal buckets")
	}
	return nil
}

func isAlnum(c byte) bool { return 'a' <= c && c <= 'z' || '0' <= c && c <= '9' }
