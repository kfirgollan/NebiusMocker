package s3endpoint_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/backend/s3endpoint"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func TestOpenNebiusWithoutFront(t *testing.T) {
	op := s3endpoint.New(s3endpoint.Config{NebiusURL: func() string { return "" }})
	_, err := op.Open(t.Context(), domain.Side{Kind: domain.ProviderNebius, Bucket: "b"})
	ee, ok := errors.AsType[*domain.EndpointError](err)
	if !ok || ee.Code != "ServiceUnavailable" {
		t.Fatalf("err = %v, want ServiceUnavailable", err)
	}
}

func TestOpenRejectsAzure(t *testing.T) {
	_, err := s3endpoint.New(s3endpoint.Config{}).Open(t.Context(), domain.Side{Kind: domain.ProviderAzureBlob})
	if err == nil {
		t.Fatal("want an error for a non-S3 provider")
	}
}

// TestNebiusRequestsAreSignedWithTheMockerClock proves requests to the
// mocker's front carry the injected clock's time, while an external endpoint
// keeps the wall clock.
func TestNebiusRequestsAreSignedWithTheMockerClock(t *testing.T) {
	var amzDate atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		amzDate.Store(r.Header.Get("X-Amz-Date"))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>nope</Message></Error>`))
	}))
	defer srv.Close()

	clk := clock.NewManual(time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC))
	op := s3endpoint.New(s3endpoint.Config{NebiusURL: func() string { return srv.URL }, Clock: clk})
	creds := domain.Credentials{AccessKeyID: "ak", Secret: "sk"}

	ep, err := op.Open(t.Context(), domain.Side{Kind: domain.ProviderNebius, Region: "eu-north1", Bucket: "b", Creds: creds})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ep.Head(t.Context(), "k")
	if errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("head err = %v, want NotFound", err)
	}
	if got := amzDate.Load(); got != "20310506T070809Z" {
		t.Errorf("X-Amz-Date = %v, want the manual clock's time", got)
	}

	ext, err := op.Open(t.Context(), domain.Side{Kind: domain.ProviderS3Compatible, Endpoint: srv.URL, Region: "us-east-1", Bucket: "b", Creds: creds})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ext.Head(t.Context(), "k"); err == nil {
		t.Fatal("want an error")
	}
	if got, _ := amzDate.Load().(string); strings.HasPrefix(got, "2031") {
		t.Errorf("external endpoint signed with the mocker clock: %s", got)
	}
}

func TestErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`))
	}))
	op := s3endpoint.New(s3endpoint.Config{})
	side := domain.Side{Kind: domain.ProviderS3Compatible, Endpoint: srv.URL, Bucket: "b", Creds: domain.Credentials{Anonymous: true}}
	ep, err := op.Open(t.Context(), side)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ep.Get(t.Context(), "k")
	if ee, ok := errors.AsType[*domain.EndpointError](err); !ok || ee.Code != "AccessDenied" || ee.Message != "denied" {
		t.Errorf("get err = %v, want AccessDenied", err)
	}

	srv.Close() // transport failure
	err = ep.Delete(t.Context(), "k")
	if ee, ok := errors.AsType[*domain.EndpointError](err); !ok || ee.Code != "RequestError" {
		t.Errorf("delete err = %v, want RequestError", err)
	}
}
