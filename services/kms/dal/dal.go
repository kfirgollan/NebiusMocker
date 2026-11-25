// Package dal implements the KMS domain repositories over pkg/store.
// It is the only KMS package that touches storage.
package dal

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// toDomain maps storage errors to domain error kinds.
func toDomain(err error, id string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return errs.NotFound(id)
	case errors.Is(err, store.ErrNameTaken):
		existing := ""
		if nt, ok := errors.AsType[*store.NameTakenError](err); ok {
			existing = nt.ExistingID
		}
		return errs.AlreadyExists(existing, "a resource with this name already exists in the parent")
	default:
		return err
	}
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

func decode[T proto.Message](rec store.Record, newMsg func() T) (T, error) {
	m := newMsg()
	if err := proto.Unmarshal(rec.Data, m); err != nil {
		var zero T
		return zero, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return m, nil
}

func encodePageToken(seq uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatUint(seq, 10)))
}

func decodePageToken(token string) (uint64, error) {
	if token == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err == nil {
		if seq, perr := strconv.ParseUint(string(b), 10, 64); perr == nil {
			return seq, nil
		}
	}
	return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
}
