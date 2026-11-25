package objstore_test

import (
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

func TestVersionMultipart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		etag string
		want bool
	}{
		{"abc", false},
		{"abc-3", true},
		{"abc-", false},
		{"", false},
		{"d41d8cd98f00b204e9800998ecf8427e", false},
	}
	for _, tc := range tests {
		if got := (objstore.Version{ETag: tc.etag}).Multipart(); got != tc.want {
			t.Errorf("Multipart(%q) = %v, want %v", tc.etag, got, tc.want)
		}
	}
}
