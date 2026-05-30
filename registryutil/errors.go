package registryutil

import (
	"errors"
	"fmt"
	"net/http"

	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

type ErrorKind string

const (
	KindValidation ErrorKind = "validation"
	KindTransport  ErrorKind = "transport"
	KindAuth       ErrorKind = "auth"
)

type Error struct {
	Kind    ErrorKind
	Op      string
	Message string
	Err     error
}

func (e *Error) Error() string {
	switch {
	case e == nil:
		return "<nil>"
	case e.Message != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Op, e.Message, e.Err)
	case e.Message != "":
		return fmt.Sprintf("%s: %s", e.Op, e.Message)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Op, e.Err)
	default:
		return e.Op
	}
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	if t.Kind != "" && e.Kind != t.Kind {
		return false
	}
	if t.Op != "" && e.Op != t.Op {
		return false
	}
	return true
}

var (
	ErrValidation = &Error{Kind: KindValidation}
	ErrTransport  = &Error{Kind: KindTransport}
	ErrAuth       = &Error{Kind: KindAuth}
)

func validationError(op, message string, err error) error {
	return &Error{Kind: KindValidation, Op: op, Message: message, Err: err}
}

func transportError(op, message string, err error) error {
	return &Error{Kind: KindTransport, Op: op, Message: message, Err: err}
}

func authError(op, message string, err error) error {
	return &Error{Kind: KindAuth, Op: op, Message: message, Err: err}
}

// IsAuthError reports whether err originated from an authentication or
// authorisation failure against a remote OCI registry.  It catches:
//   - HTTP 401 / 403 responses surfaced as *errcode.ErrorResponse
//   - auth.ErrBasicCredentialNotFound (empty credential for Basic challenge)
func IsAuthError(err error) bool {
	if errors.Is(err, auth.ErrBasicCredentialNotFound) {
		return true
	}
	var errResp *errcode.ErrorResponse
	return errors.As(err, &errResp) &&
		(errResp.StatusCode == http.StatusUnauthorized || errResp.StatusCode == http.StatusForbidden)
}
