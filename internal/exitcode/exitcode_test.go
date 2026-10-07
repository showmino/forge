package exitcode

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
)

func TestFrom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want Code
	}{
		{"plain", errors.New("boom"), Unexpected},
		{"typed", Usagef("bad flag"), Usage},
		{"wrapped typed", fmt.Errorf("ctx: %w", New(NotFound, "x")), NotFound},
		{"deadline", context.DeadlineExceeded, Timeout},
		{"not found", connect.NewError(connect.CodeNotFound, errors.New("no")), NotFound},
		{"unauthenticated", connect.NewError(connect.CodeUnauthenticated, nil), Unauthenticated},
		{"denied", connect.NewError(connect.CodePermissionDenied, nil), PermissionDenied},
		{"exists", connect.NewError(connect.CodeAlreadyExists, nil), Conflict},
		{"precondition", connect.NewError(connect.CodeFailedPrecondition, nil), Conflict},
		{"invalid", connect.NewError(connect.CodeInvalidArgument, nil), Usage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := From(tt.err).Code; got != tt.want {
				t.Errorf("From(%v).Code = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
	if From(nil) != nil {
		t.Error("From(nil) should be nil")
	}
}

func TestUnauthenticatedHint(t *testing.T) {
	t.Parallel()
	e := From(connect.NewError(connect.CodeUnauthenticated, errors.New("token expired")))
	if e.Hint == "" || e.Error() != "token expired" {
		t.Errorf("got %q hint %q", e.Error(), e.Hint)
	}
}
