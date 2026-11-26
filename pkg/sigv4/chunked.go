package sigv4

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maxChunk bounds one aws-chunked chunk; SDKs use 8–64 KiB chunks.
const maxChunk = 16 << 20

// maxTrailers bounds the trailing headers of one body (SDKs send one checksum).
const maxTrailers = 16

// trailerSignature is the trailing header that signs the other trailers
// (STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER).
const trailerSignature = "X-Amz-Trailer-Signature"

// ErrBadChunk reports a malformed aws-chunked body (IncompleteBody / InvalidRequest).
var ErrBadChunk = errors.New("sigv4: malformed aws-chunked body")

// NewChunkedReader decodes an aws-chunked request body (a.Streaming() is
// true). For the signed modes every chunk signature is verified, chained
// from the request signature; a mismatch fails the read with
// ErrSignatureMismatch. Trailing headers (checksums) are collected and, in
// the signed trailer mode, verified against x-amz-trailer-signature;
// Trailer returns them once the body has been read to EOF.
func NewChunkedReader(body io.Reader, a Auth, secret string) (*ChunkedReader, error) {
	switch a.PayloadHash {
	case StreamingSigned, StreamingSignedTrailer, StreamingUnsigned:
	default:
		return nil, fmt.Errorf("%w: unsupported payload mode %s", ErrMalformed, a.PayloadHash)
	}
	return &ChunkedReader{
		r:       bufio.NewReader(body),
		auth:    a,
		signed:  a.PayloadHash != StreamingUnsigned,
		trailer: a.PayloadHash != StreamingSigned,
		key:     SigningKey(secret, a.Date, a.Region, a.Service),
		prevSig: a.Signature,
	}, nil
}

// ChunkedReader is the decoded payload of an aws-chunked body.
type ChunkedReader struct {
	r       *bufio.Reader
	auth    Auth
	signed  bool
	trailer bool
	key     []byte
	prevSig string

	chunkBuf  []byte // signed mode: the verified current chunk
	remaining int64  // unsigned mode: bytes left in the current chunk
	done      bool
	err       error
	trailers  http.Header // the verified trailing headers, once read
}

// Trailer returns the trailing headers (e.g. X-Amz-Checksum-Crc32), with
// canonical names and without the trailer signature. It is nil until Read
// has returned io.EOF, and empty when the body had none.
func (c *ChunkedReader) Trailer() http.Header {
	if !c.done || c.err != nil {
		return nil
	}
	return c.trailers
}

func (c *ChunkedReader) Read(p []byte) (int, error) {
	for {
		if c.err != nil {
			return 0, c.err
		}
		if len(c.chunkBuf) > 0 {
			n := copy(p, c.chunkBuf)
			c.chunkBuf = c.chunkBuf[n:]
			return n, nil
		}
		if c.remaining > 0 {
			return c.readUnsigned(p)
		}
		if c.done {
			return 0, io.EOF
		}
		c.err = c.nextChunk()
	}
}

func (c *ChunkedReader) readUnsigned(p []byte) (int, error) {
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	if c.remaining == 0 && err == nil {
		err = c.expectCRLF()
	}
	if errors.Is(err, io.EOF) {
		err = fmt.Errorf("%w: body ended inside a chunk", ErrBadChunk)
	}
	if err != nil {
		c.err = err
	}
	return n, nil
}

// nextChunk reads one chunk header (and, when signed, its data).
func (c *ChunkedReader) nextChunk() error {
	line, err := c.readLine()
	if err != nil {
		return err
	}
	sizeHex, ext, _ := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
	if err != nil || size < 0 || size > maxChunk {
		return fmt.Errorf("%w: bad chunk size %q", ErrBadChunk, sizeHex)
	}
	if c.signed {
		if err := c.readSignedChunk(ext, size); err != nil {
			return err
		}
	} else {
		c.remaining = size
	}
	if size == 0 {
		c.done = true
		return c.readTrailers()
	}
	return nil
}

// readSignedChunk reads and verifies one signed chunk into chunkBuf.
func (c *ChunkedReader) readSignedChunk(ext string, size int64) error {
	sig, ok := strings.CutPrefix(ext, "chunk-signature=")
	if !ok {
		return fmt.Errorf("%w: missing chunk-signature", ErrBadChunk)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(c.r, data); err != nil {
		return fmt.Errorf("%w: %w", ErrBadChunk, err)
	}
	if err := c.verifyChunk(sig, data); err != nil {
		return err
	}
	if size > 0 {
		if err := c.expectCRLF(); err != nil {
			return err
		}
	}
	c.chunkBuf = data
	return nil
}

func (c *ChunkedReader) verifyChunk(sig string, data []byte) error {
	sum := sha256.Sum256(data)
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.auth.AmzDate.Format(amzDateLayout) + "\n" + c.auth.Scope() + "\n" +
		c.prevSig + "\n" + emptySHA256 + "\n" + hex.EncodeToString(sum[:])
	want := Sign(c.key, sts)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return ErrSignatureMismatch
	}
	c.prevSig = sig
	return nil
}

// readTrailers consumes the trailing headers after the final chunk, up to
// the terminating empty line (or EOF), and verifies their signature in the
// signed trailer mode.
func (c *ChunkedReader) readTrailers() error {
	trailers := http.Header{}
	if !c.trailer {
		// STREAMING-AWS4-HMAC-SHA256-PAYLOAD ends with "0;chunk-signature=…\r\n\r\n".
		_, err := c.readLine()
		if errors.Is(err, io.EOF) {
			err = nil
		}
		c.trailers = trailers
		return err
	}
	var canonical strings.Builder // "name:value\n" per trailer, as signed
	sig := ""
	for n := 0; ; n++ {
		line, err := c.readLine()
		if errors.Is(err, io.EOF) || (err == nil && line == "") {
			break
		}
		if err != nil {
			return err
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)
		if !ok || name == "" || n >= maxTrailers {
			return fmt.Errorf("%w: bad trailing header %q", ErrBadChunk, line)
		}
		if http.CanonicalHeaderKey(name) == trailerSignature {
			sig = value
			continue
		}
		trailers.Add(name, value)
		canonical.WriteString(name + ":" + value + "\n")
	}
	if c.signed {
		if err := c.verifyTrailer(sig, canonical.String()); err != nil {
			return err
		}
	}
	c.trailers = trailers
	return nil
}

// verifyTrailer checks x-amz-trailer-signature, chained from the final
// chunk's signature over the canonical trailing headers.
func (c *ChunkedReader) verifyTrailer(sig, canonical string) error {
	if sig == "" {
		return fmt.Errorf("%w: missing x-amz-trailer-signature", ErrSignatureMismatch)
	}
	sum := sha256.Sum256([]byte(canonical))
	sts := "AWS4-HMAC-SHA256-TRAILER\n" + c.auth.AmzDate.Format(amzDateLayout) + "\n" + c.auth.Scope() + "\n" +
		c.prevSig + "\n" + hex.EncodeToString(sum[:])
	if !hmac.Equal([]byte(Sign(c.key, sts)), []byte(sig)) {
		return ErrSignatureMismatch
	}
	return nil
}

func (c *ChunkedReader) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			return "", io.EOF
		}
		return "", fmt.Errorf("%w: %w", ErrBadChunk, err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *ChunkedReader) expectCRLF() error {
	b := make([]byte, 2)
	if _, err := io.ReadFull(c.r, b); err != nil || string(b) != "\r\n" {
		return fmt.Errorf("%w: missing CRLF after chunk data", ErrBadChunk)
	}
	return nil
}
