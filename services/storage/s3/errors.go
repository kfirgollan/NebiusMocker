package s3

import (
	"encoding/xml"
	"errors"
	"net/http"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/sigv4"
)

// s3Error is an S3 REST error response.
type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	Region    string   `xml:"Region,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
	status    int
}

// codedError is an S3 error with a specific code, raised by the front itself.
type codedError struct {
	code   string
	status int
	msg    string
}

func (e *codedError) Error() string { return e.msg }

func notImplemented(msg string) error {
	return &codedError{code: "NotImplemented", status: http.StatusNotImplemented, msg: msg}
}

func invalidArgument(msg string) error {
	return &codedError{code: "InvalidArgument", status: http.StatusBadRequest, msg: msg}
}

func coded(code string, status int, msg string) error {
	return &codedError{code: code, status: status, msg: msg}
}

// Storage-class violations are reported as InvalidStorageClass.
const storageClassField = "x-amz-storage-class"

// toS3Error maps sigv4 and domain errors to S3 error codes.
func toS3Error(err error) s3Error {
	if ce, ok := errors.AsType[*codedError](err); ok {
		return s3Error{Code: ce.code, Message: ce.msg, status: ce.status}
	}
	if xe, ok := errors.AsType[*xmlError](err); ok {
		return s3Error{Code: xe.Code, Message: xe.Message, status: http.StatusBadRequest}
	}
	if re, ok := errors.AsType[*sigv4.RegionError](err); ok {
		code := "AuthorizationHeaderMalformed"
		if re.Presigned {
			code = "AuthorizationQueryParametersError"
		}
		return s3Error{Code: code, Message: re.Error(), Region: re.Want, status: http.StatusBadRequest}
	}
	switch {
	case errors.Is(err, sigv4.ErrMissingAuth):
		return s3Error{Code: "AccessDenied", Message: "Access Denied", status: http.StatusForbidden}
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
	case errs.KindQuotaExceeded:
		return s3Error{Code: "QuotaExceeded", Message: domainMessage(err), status: http.StatusBadRequest}
	case errs.KindAlreadyExists:
		return s3Error{Code: "BucketAlreadyExists", Message: "The requested bucket name is not available. The bucket namespace is shared by all users of the system. Please select a different name and try again.", status: http.StatusConflict}
	case errs.KindBadRequest:
		if de, ok := errors.AsType[*errs.Error](err); ok && len(de.Violations) == 1 && de.Violations[0].Field == storageClassField {
			return s3Error{Code: "InvalidStorageClass", Message: de.Violations[0].Message, status: http.StatusBadRequest}
		}
		return s3Error{Code: "InvalidArgument", Message: domainMessage(err), status: http.StatusBadRequest}
	case errs.KindOutOfRange:
		return s3Error{Code: "InvalidArgument", Message: domainMessage(err), status: http.StatusBadRequest}
	case errs.KindBadState:
		return s3Error{Code: "InvalidBucketState", Message: domainMessage(err), status: http.StatusConflict}
	case errs.KindUnknown, errs.KindConflict, errs.KindOperationConflict, errs.KindInternal:
		return internalError()
	default:
		return internalError()
	}
}

// domainMessage renders a domain error with its field violations.
func domainMessage(err error) string {
	de, ok := errors.AsType[*errs.Error](err)
	if !ok {
		return err.Error()
	}
	if len(de.Violations) == 0 {
		return de.Message
	}
	parts := make([]string, 0, len(de.Violations))
	for _, v := range de.Violations {
		parts = append(parts, v.Field+": "+v.Message)
	}
	return strings.Join(parts, "; ")
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
	writeRaw(w, status, append([]byte(xml.Header), out...))
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write(body) //nolint:gosec // XML-encoded S3 response (application/xml), not HTML; a gone client needs no handling
}
