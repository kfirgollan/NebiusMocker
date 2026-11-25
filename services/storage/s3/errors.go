package s3

import (
	"encoding/xml"
	"errors"
	"net/http"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/sigv4"
)

// s3Error is an S3 REST error response.
type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
	status    int
}

// notImplementedError marks S3 operations the mocker does not serve.
type notImplementedError struct{ msg string }

func (e *notImplementedError) Error() string { return e.msg }

func notImplemented(msg string) error { return &notImplementedError{msg: msg} }

// toS3Error maps sigv4 and domain errors to S3 error codes.
func toS3Error(err error) s3Error {
	if ni, ok := errors.AsType[*notImplementedError](err); ok {
		return s3Error{Code: "NotImplemented", Message: ni.msg, status: http.StatusNotImplemented}
	}
	switch {
	case errors.Is(err, sigv4.ErrMissingAuth):
		return s3Error{Code: "AccessDenied", Message: "Anonymous access is not supported; sign requests with a Nebius access key", status: http.StatusForbidden}
	case errors.Is(err, sigv4.ErrSignatureMismatch):
		return s3Error{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided.", status: http.StatusForbidden}
	case errors.Is(err, sigv4.ErrTimeSkewed):
		return s3Error{Code: "RequestTimeTooSkewed", Message: "The difference between the request time and the server's time is too large.", status: http.StatusForbidden}
	case errors.Is(err, sigv4.ErrExpired):
		return s3Error{Code: "AccessDenied", Message: "Request has expired", status: http.StatusForbidden}
	case errors.Is(err, sigv4.ErrMalformed):
		return s3Error{Code: "AuthorizationHeaderMalformed", Message: err.Error(), status: http.StatusBadRequest}
	case errors.Is(err, sigv4.ErrBadChunk):
		return s3Error{Code: "IncompleteBody", Message: err.Error(), status: http.StatusBadRequest}
	}
	switch errs.KindOf(err) {
	case errs.KindUnauthenticated:
		return s3Error{Code: "InvalidAccessKeyId", Message: "The access key ID you provided does not exist or is not active.", status: http.StatusForbidden}
	case errs.KindNotFound:
		return s3Error{Code: "NoSuchBucket", Message: "The specified bucket does not exist", status: http.StatusNotFound}
	case errs.KindPermissionDenied:
		return s3Error{Code: "AccessDenied", Message: "Access Denied", status: http.StatusForbidden}
	case errs.KindBadRequest:
		return s3Error{Code: "InvalidRequest", Message: err.Error(), status: http.StatusBadRequest}
	case errs.KindUnknown, errs.KindAlreadyExists, errs.KindBadState, errs.KindConflict, errs.KindOperationConflict,
		errs.KindQuotaExceeded, errs.KindInternal:
		return internalError()
	default:
		return internalError()
	}
}

func internalError() s3Error {
	return s3Error{Code: "InternalError", Message: "We encountered an internal error. Please try again.", status: http.StatusInternalServerError}
}

func writeXML(w http.ResponseWriter, status int, v any) {
	out, err := xml.Marshal(v)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header)) // client gone: nothing to do
	_, _ = w.Write(out)                //nolint:gosec // XML-encoded S3 response (application/xml), not HTML
}
