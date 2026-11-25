package domain_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// TestResolvePayload covers the privileged in-process read behind
// peer.SecretPayloads.
func TestResolvePayload(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	first := []*mysteryboxpb.Payload{strEntry("AUTH_TOKEN", "t1"), binEntry("bin", []byte{0, 1})}
	sec := e.newSecret(t, "s", first...)
	v1 := e.primary(t, sec)
	v2 := e.newVersion(t, sec, false, strEntry("AUTH_TOKEN", "t2"))
	other := e.newSecret(t, "other", strEntry("k", "v"))
	empty := e.newSecret(t, "empty")
	gone := e.newVersion(t, other, false, strEntry("k", "old"))
	done(t)(e.svc.DeleteVersion(ctx, gone, nil))
	deletedSecret := e.newSecret(t, "deleted", strEntry("k", "v"))
	done(t)(e.svc.DeleteSecret(ctx, deletedSecret, nil))

	ok := []struct {
		name       string
		ref        domain.PayloadRef
		wantSecret string
		wantVer    string
	}{
		{"secret only uses primary", domain.PayloadRef{SecretID: sec}, sec, v1},
		{"both IDs", domain.PayloadRef{SecretID: sec, VersionID: v2}, sec, v2},
		{"version only fills secret", domain.PayloadRef{VersionID: v2}, sec, v2},
	}
	for _, tt := range ok {
		got, err := e.svc.ResolvePayload(ctx, tt.ref)
		if err != nil {
			t.Fatalf("%s: ResolvePayload: %v", tt.name, err)
		}
		if got.SecretID != tt.wantSecret || got.VersionID != tt.wantVer {
			t.Errorf("%s: resolved %s/%s, want %s/%s", tt.name, got.SecretID, got.VersionID, tt.wantSecret, tt.wantVer)
		}
	}
	got, err := e.svc.ResolvePayload(ctx, domain.PayloadRef{SecretID: sec})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(first, got.Entries, protocmp.Transform()); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}

	bad := []struct {
		name string
		ref  domain.PayloadRef
		want errs.Kind
	}{
		{"no IDs", domain.PayloadRef{}, errs.KindBadRequest},
		{"unknown secret", domain.PayloadRef{SecretID: "mbsec-e00mock0000000099999"}, errs.KindNotFound},
		{"unknown version", domain.PayloadRef{VersionID: "mbsecver-e00mock0000000099999"}, errs.KindNotFound},
		{"version of another secret", domain.PayloadRef{SecretID: other, VersionID: v2}, errs.KindNotFound},
		{"no primary", domain.PayloadRef{SecretID: empty}, errs.KindBadState},
		{"deleted version", domain.PayloadRef{VersionID: gone}, errs.KindBadState},
		{"deleted secret", domain.PayloadRef{SecretID: deletedSecret}, errs.KindBadState},
	}
	for _, tt := range bad {
		if _, err := e.svc.ResolvePayload(ctx, tt.ref); errs.KindOf(err) != tt.want {
			t.Errorf("%s: err = %v, want kind %v", tt.name, err, tt.want)
		}
	}

	// A deleted primary is unusable.
	done(t)(e.svc.DeleteVersion(ctx, v1, nil))
	if _, err := e.svc.ResolvePayload(ctx, domain.PayloadRef{SecretID: sec}); errs.KindOf(err) != errs.KindBadState {
		t.Errorf("deleted primary: err = %v, want bad_state", err)
	}
}
