package registryutil

import (
	"errors"
	"fmt"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

func TestError_ErrorString_AllBranches(t *testing.T) {
	wrapped := fmt.Errorf("cause")
	cases := []struct {
		e    *Error
		want string
	}{
		{nil, "<nil>"},
		{&Error{Op: "op", Message: "msg", Err: wrapped}, "op: msg: cause"},
		{&Error{Op: "op", Message: "msg"}, "op: msg"},
		{&Error{Op: "op", Err: wrapped}, "op: cause"},
		{&Error{Op: "op"}, "op"},
	}
	for _, c := range cases {
		got := c.e.Error()
		if got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}

func TestError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("cause")
	e := &Error{Err: cause}
	if e.Unwrap() != cause {
		t.Fatalf("Unwrap: expected %v, got %v", cause, e.Unwrap())
	}
	var nilErr *Error
	if nilErr.Unwrap() != nil {
		t.Fatal("nil Error.Unwrap() must return nil")
	}
}

func TestError_Is_KindMismatch(t *testing.T) {
	e := &Error{Kind: KindValidation, Op: "op"}
	target := &Error{Kind: KindTransport}
	if errors.Is(e, target) {
		t.Fatal("Is() must return false when kinds differ")
	}
}

func TestError_Is_OpMismatch(t *testing.T) {
	e := &Error{Kind: KindTransport, Op: "op1"}
	target := &Error{Kind: KindTransport, Op: "op2"}
	if errors.Is(e, target) {
		t.Fatal("Is() must return false when ops differ")
	}
}

func TestError_Is_NonErrorTarget(t *testing.T) {
	e := &Error{Kind: KindTransport}
	if errors.Is(e, fmt.Errorf("other")) {
		t.Fatal("Is() must return false for non-*Error target")
	}
}

func TestValidationError_IsErrValidation(t *testing.T) {
	err := validationError("op", "msg", nil)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestTransportError_IsErrTransport(t *testing.T) {
	err := transportError("op", "msg", fmt.Errorf("cause"))
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("expected ErrTransport, got %v", err)
	}
}

func TestError_Unwrap_ChainedErrors(t *testing.T) {
	inner := fmt.Errorf("inner")
	outer := transportError("op", "msg", inner)
	if !errors.Is(outer, inner) {
		t.Fatalf("expected errors.Is to find inner via Unwrap chain, got %v", outer)
	}
}

func TestAuthError_IsErrAuth(t *testing.T) {
	err := authError("op", "msg", fmt.Errorf("cause"))
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic error", fmt.Errorf("generic"), false},
		{"401 Unauthorized", &errcode.ErrorResponse{StatusCode: 401}, true},
		{"403 Forbidden", &errcode.ErrorResponse{StatusCode: 403}, true},
		{"404 Not Found", &errcode.ErrorResponse{StatusCode: 404}, false},
		{"500 Internal Server Error", &errcode.ErrorResponse{StatusCode: 500}, false},
		{"wrapped 401", fmt.Errorf("outer: %w", &errcode.ErrorResponse{StatusCode: 401}), true},
		{"wrapped 403", fmt.Errorf("outer: %w", &errcode.ErrorResponse{StatusCode: 403}), true},
		{"ErrBasicCredentialNotFound", auth.ErrBasicCredentialNotFound, true},
		{"wrapped ErrBasicCredentialNotFound", fmt.Errorf("HEAD: %w", auth.ErrBasicCredentialNotFound), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAuthError(c.err); got != c.want {
				t.Fatalf("IsAuthError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
