package catalogutil

import "fmt"

type ErrorKind string

const (
	KindNotFound   ErrorKind = "not_found"
	KindValidation ErrorKind = "validation"
	KindTransport  ErrorKind = "transport"
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
	ErrNotFound   = &Error{Kind: KindNotFound}
	ErrValidation = &Error{Kind: KindValidation}
	ErrTransport  = &Error{Kind: KindTransport}
)

func validationError(op, message string, err error) error {
	return &Error{Kind: KindValidation, Op: op, Message: message, Err: err}
}

func transportError(op, message string, err error) error {
	return &Error{Kind: KindTransport, Op: op, Message: message, Err: err}
}
