package sigv4

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxChunk bounds one aws-chunked chunk; SDKs use 8–64 KiB chunks.
const maxChunk = 16 << 20

// ErrBadChunk reports a malformed aws-chunked body (IncompleteBody / InvalidRequest).
var ErrBadChunk = errors.New("sigv4: malformed aws-chunked body")

// NewChunkedReader decodes an aws-chunked request body (a.Streaming() is
// true). For the signed modes every chunk signature is verified, chained
// from the request signature; a mismatch fails the read with
// ErrSignatureMismatch. Trailing headers (checksums) are consumed and dropped.
func NewChunkedReader(body io.Reader, a Auth, secret string) (io.Reader, error) {
	switch a.PayloadHash {
	case StreamingSigned, StreamingSignedTrailer, StreamingUnsigned:
	default:
		return nil, fmt.Errorf("%w: unsupported payload mode %s", ErrMalformed, a.PayloadHash)
	}
	return &chunkedReader{
		r:        bufio.NewReader(body),
		auth:     a,
		signed:   a.PayloadHash != StreamingUnsigned,
		trailer:  a.PayloadHash != StreamingSigned,
		key:      SigningKey(secret, a.Date, a.Region, a.Service),
		prevSig:  a.Signature,
		chunkBuf: nil,
	}, nil
}

type chunkedReader struct {
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
}

func (c *chunkedReader) Read(p []byte) (int, error) {
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

func (c *chunkedReader) readUnsigned(p []byte) (int, error) {
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
func (c *chunkedReader) nextChunk() error {
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
func (c *chunkedReader) readSignedChunk(ext string, size int64) error {
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

func (c *chunkedReader) verifyChunk(sig string, data []byte) error {
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
// the terminating empty line (or EOF).
func (c *chunkedReader) readTrailers() error {
	if !c.trailer {
		// STREAMING-AWS4-HMAC-SHA256-PAYLOAD ends with "0;chunk-signature=…\r\n\r\n".
		_, err := c.readLine()
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	for {
		line, err := c.readLine()
		if errors.Is(err, io.EOF) || (err == nil && line == "") {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (c *chunkedReader) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			return "", io.EOF
		}
		return "", fmt.Errorf("%w: %w", ErrBadChunk, err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *chunkedReader) expectCRLF() error {
	b := make([]byte, 2)
	if _, err := io.ReadFull(c.r, b); err != nil || string(b) != "\r\n" {
		return fmt.Errorf("%w: missing CRLF after chunk data", ErrBadChunk)
	}
	return nil
}
