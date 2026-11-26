//go:build e2e_storage

package azblob_test

import (
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/backend/azblob"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
	"github.com/kfirgollan/NebiusMocker/test/azuritefixture"
)

func TestMain(m *testing.M) { os.Exit(azuritefixture.Main(m)) }

func azSide(srv *azuritefixture.Server, ctr string, creds domain.Credentials) domain.Side {
	return domain.Side{Kind: domain.ProviderAzureBlob, Endpoint: srv.Endpoint, Bucket: ctr, Creds: creds}
}

func azKeyCreds(srv *azuritefixture.Server) domain.Credentials {
	return domain.Credentials{AzureAccount: srv.Account, AzureKey: srv.Key}
}

func TestSource(t *testing.T) {
	srv := azuritefixture.Start(t)
	ctx := t.Context()
	const ctr = "source"
	srv.CreateContainer(t, ctr)
	srv.Upload(t, ctr, "docs/a.txt", []byte("alpha"), azuritefixture.BlobOptions{
		ContentType: "text/plain", CacheControl: "max-age=60", ContentDisposition: "inline", ContentLanguage: "en",
		Metadata: map[string]string{"Owner": "team-a"},
	})
	srv.Upload(t, ctr, "docs/sub/b.bin", []byte("bravo!"), azuritefixture.BlobOptions{})
	srv.Upload(t, ctr, "other/c.txt", []byte("c"), azuritefixture.BlobOptions{})

	ep, err := azblob.New(nil).Open(ctx, azSide(srv, ctr, azKeyCreds(srv)))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("list", func(t *testing.T) {
		var keys []string
		for m, err := range ep.List(ctx, "docs/") {
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, m.Key)
			if m.Key == "docs/a.txt" {
				if m.Size != 5 || m.ETag == "" || m.LastModified.IsZero() || m.ContentType != "text/plain" || m.Metadata["owner"] != "team-a" {
					t.Errorf("list meta = %+v", m)
				}
			}
		}
		if !slices.Equal(keys, []string{"docs/a.txt", "docs/sub/b.bin"}) {
			t.Errorf("keys = %v", keys)
		}
	})

	t.Run("head and get", func(t *testing.T) {
		h, err := ep.Head(ctx, "docs/a.txt")
		if err != nil {
			t.Fatal(err)
		}
		if h.Size != 5 || h.ContentType != "text/plain" || h.CacheControl != "max-age=60" || h.ContentDisposition != "inline" ||
			h.ContentLanguage != "en" || h.Metadata["owner"] != "team-a" || h.ETag == "" || h.LastModified.IsZero() {
			t.Errorf("head = %+v", h)
		}
		rc, gm, err := ep.Get(ctx, "docs/a.txt")
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		body, err := io.ReadAll(rc)
		if err != nil || string(body) != "alpha" {
			t.Fatalf("body = %q, %v", body, err)
		}
		if gm.Size != 5 || gm.ETag != h.ETag || gm.ContentType != "text/plain" || gm.Metadata["owner"] != "team-a" {
			t.Errorf("get meta = %+v", gm)
		}
	})

	t.Run("not found", func(t *testing.T) {
		if _, err := ep.Head(ctx, "missing"); !isNotFound(err) {
			t.Errorf("head err = %v", err)
		}
		if _, _, err := ep.Get(ctx, "missing"); !isNotFound(err) {
			t.Errorf("get err = %v", err)
		}
	})

	t.Run("missing container", func(t *testing.T) {
		bad, err := azblob.New(nil).Open(ctx, azSide(srv, "nosuch", azKeyCreds(srv)))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, err := range bad.List(ctx, "") {
			n++
			assertEndpointCode(t, err, "ContainerNotFound")
		}
		if n != 1 {
			t.Errorf("list yielded %d results, want one error", n)
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		wrong := azKeyCreds(srv)
		wrong.AzureKey = "d3Jvbmcta2V5LXdyb25nLWtleS13cm9uZy1rZXktd3Jvbmcta2V5LXdyb25nLWtleS13cm9uZy1rZXktd3Jvbmcta2V5LXdyb25nLWtleQ=="
		bad, err := azblob.New(nil).Open(ctx, azSide(srv, ctr, wrong))
		if err != nil {
			t.Fatal(err)
		}
		_, err = bad.Head(ctx, "docs/a.txt")
		ee, ok := errors.AsType[*domain.EndpointError](err)
		if !ok || (ee.Code != "AuthenticationFailed" && ee.Code != "AuthorizationFailure") {
			t.Errorf("wrong key err = %v", err)
		}
	})

	t.Run("write is not implemented", func(t *testing.T) {
		assertEndpointCode(t, ep.Delete(ctx, "docs/a.txt"), "NotImplemented")
		_, err := ep.Put(ctx, "x", nil, domain.ObjectMeta{})
		assertEndpointCode(t, err, "NotImplemented")
		_, err = ep.ReplaceMetadata(ctx, "x", domain.ObjectMeta{})
		assertEndpointCode(t, err, "NotImplemented")
	})
}

func TestAnonymousPublicContainer(t *testing.T) {
	srv := azuritefixture.Start(t)
	ctx := t.Context()
	srv.CreatePublicContainer(t, "public")
	srv.CreateContainer(t, "private")
	srv.Upload(t, "public", "open.txt", []byte("open"), azuritefixture.BlobOptions{ContentType: "text/plain"})
	srv.Upload(t, "private", "closed.txt", []byte("closed"), azuritefixture.BlobOptions{})
	anon := domain.Credentials{Anonymous: true}

	ep, err := azblob.New(nil).Open(ctx, azSide(srv, "public", anon))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for m, err := range ep.List(ctx, "") {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, m.Key)
	}
	if !slices.Equal(keys, []string{"open.txt"}) {
		t.Errorf("keys = %v", keys)
	}
	rc, _, err := ep.Get(ctx, "open.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if body, _ := io.ReadAll(rc); string(body) != "open" {
		t.Errorf("body = %q", body)
	}

	priv, err := azblob.New(nil).Open(ctx, azSide(srv, "private", anon))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = priv.Get(ctx, "closed.txt")
	if _, ok := errors.AsType[*domain.EndpointError](err); !ok {
		t.Errorf("anonymous read of a private container: err = %v, want EndpointError", err)
	}
}

func isNotFound(err error) bool { return errs.KindOf(err) == errs.KindNotFound }

func assertEndpointCode(t *testing.T, err error, code string) {
	t.Helper()
	ee, ok := errors.AsType[*domain.EndpointError](err)
	if !ok || ee.Code != code {
		t.Errorf("err = %v, want EndpointError %s", err, code)
	}
}
