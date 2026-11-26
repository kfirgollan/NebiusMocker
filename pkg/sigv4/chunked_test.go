package sigv4_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/sigv4"
)

const ckSeed = "seedsignature0000000000000000000000000000000000000000000000000000"

func ckAuth(mode string) sigv4.Auth {
	return sigv4.Auth{
		AccessKeyID: keyID, Date: "20261125", Region: region, Service: "s3",
		Signature: ckSeed, AmzDate: signedAt, PayloadHash: mode,
	}
}

func ckKey() []byte { return sigv4.SigningKey(secret, "20261125", region, "s3") }

const ckScope = "20261125/" + region + "/s3/aws4_request"

func ckChunkSig(prev string, data []byte) string {
	return sigv4.Sign(ckKey(), "AWS4-HMAC-SHA256-PAYLOAD\n20261125T120000Z\n"+ckScope+"\n"+prev+"\n"+hashOf(nil)+"\n"+hashOf(data))
}

func ckTrailerSig(prev, canonical string) string {
	return sigv4.Sign(ckKey(), "AWS4-HMAC-SHA256-TRAILER\n20261125T120000Z\n"+ckScope+"\n"+prev+"\n"+hashOf([]byte(canonical)))
}

// ckSignedTrailerBody builds a STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER
// body: signed chunks, then trailer lines and the trailer signature line
// (omitted when sigLine is "").
func ckSignedTrailerBody(chunks [][]byte, trailers string, sigLine func(finalSig string) string) []byte {
	var b bytes.Buffer
	prev := ckSeed
	for _, c := range append(chunks, nil) {
		sig := ckChunkSig(prev, c)
		fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(c), sig)
		if len(c) > 0 {
			b.Write(c)
			b.WriteString("\r\n")
		}
		prev = sig
	}
	b.WriteString(trailers)
	if line := sigLine(prev); line != "" {
		b.WriteString(line)
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

func ckRead(t *testing.T, body []byte, a sigv4.Auth) (*sigv4.ChunkedReader, string, error) {
	t.Helper()
	cr, err := sigv4.NewChunkedReader(bytes.NewReader(body), a, secret)
	if err != nil {
		t.Fatalf("NewChunkedReader: %v", err)
	}
	if cr.Trailer() != nil {
		t.Fatal("Trailer() before EOF is not nil")
	}
	got, err := io.ReadAll(cr)
	return cr, string(got), err
}

func TestChunkedUnsignedTrailerIsExposed(t *testing.T) {
	t.Parallel()
	body := "5\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:DUoRhQ==\r\n\r\n"
	cr, got, err := ckRead(t, []byte(body), ckAuth(sigv4.StreamingUnsigned))
	if err != nil || got != "hello world" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if v := cr.Trailer().Get("X-Amz-Checksum-Crc32"); v != "DUoRhQ==" || len(cr.Trailer()) != 1 {
		t.Errorf("Trailer() = %v, want only x-amz-checksum-crc32: DUoRhQ==", cr.Trailer())
	}
}

func TestChunkedSignedHasNoTrailer(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	sig := ckChunkSig(ckSeed, []byte("abc"))
	fmt.Fprintf(&b, "3;chunk-signature=%s\r\nabc\r\n0;chunk-signature=%s\r\n\r\n", sig, ckChunkSig(sig, nil))
	cr, got, err := ckRead(t, b.Bytes(), ckAuth(sigv4.StreamingSigned))
	if err != nil || got != "abc" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if h := cr.Trailer(); h == nil || len(h) != 0 {
		t.Errorf("Trailer() = %v, want empty", h)
	}
}

func TestChunkedSignedTrailer(t *testing.T) {
	t.Parallel()
	chunks := [][]byte{bytes.Repeat([]byte("a"), 70), []byte("tail")}
	const trailers = "x-amz-checksum-crc32c:sOO8/Q==\r\n"
	good := func(final string) string {
		return "x-amz-trailer-signature:" + ckTrailerSig(final, "x-amz-checksum-crc32c:sOO8/Q==\n") + "\r\n"
	}
	tests := []struct {
		name     string
		trailers string
		sigLine  func(string) string
		wantErr  error
	}{
		{"valid", trailers, good, nil},
		{"tampered value", "x-amz-checksum-crc32c:AAAAAA==\r\n", good, sigv4.ErrSignatureMismatch},
		{"wrong signature", trailers, func(string) string { return "x-amz-trailer-signature:" + strings.Repeat("0", 64) + "\r\n" }, sigv4.ErrSignatureMismatch},
		{"missing signature", trailers, func(string) string { return "" }, sigv4.ErrSignatureMismatch},
		{"malformed line", "no-colon-here\r\n", good, sigv4.ErrBadChunk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := ckSignedTrailerBody(chunks, tt.trailers, tt.sigLine)
			cr, got, err := ckRead(t, body, ckAuth(sigv4.StreamingSignedTrailer))
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
				t.Fatalf("read err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if cr.Trailer() != nil {
					t.Errorf("Trailer() after a failed read = %v, want nil", cr.Trailer())
				}
				return
			}
			if got != strings.Repeat("a", 70)+"tail" {
				t.Errorf("body = %q", got)
			}
			if h := cr.Trailer(); h.Get("X-Amz-Checksum-Crc32c") != "sOO8/Q==" || h.Get("X-Amz-Trailer-Signature") != "" {
				t.Errorf("Trailer() = %v, want the checksum without the signature", h)
			}
		})
	}
}
