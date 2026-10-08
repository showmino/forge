package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
)

func opServer(t *testing.T) (*Operations, forgev1connect.OperationServiceClient) {
	t.Helper()
	ops := NewOperations(nil)
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Groups: []string{"dev"}},
		},
		Authorizer: Authorizer{WriterGroups: []string{"platform"}},
		Operations: ops,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return ops, forgev1connect.NewOperationServiceClient(srv.Client(), srv.URL)
}

func as[T any](token string, msg *T) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer "+token)
	return r
}

func TestOperationServiceRPCs(t *testing.T) {
	t.Parallel()
	ops, c := opServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gate := make(chan struct{})
	op := ops.Start(ctx, "test", "things/a", "ana", func(ctx context.Context, emit func(string, string)) error {
		emit("one", "a")
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	got, err := c.GetOperation(ctx, as("reader", &forgev1.GetOperationRequest{Name: op.GetName()}))
	if err != nil || got.Msg.GetOperation().GetKind() != "test" {
		t.Fatalf("get: %v %v", got, err)
	}
	list, err := c.ListOperations(ctx, as("reader", &forgev1.ListOperationsRequest{Running: true}))
	if err != nil || len(list.Msg.GetOperations()) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}

	for _, tc := range []struct {
		name string
		code connect.Code
	}{
		{"operations/op_000000000000", connect.CodeNotFound},
		{"op_123", connect.CodeInvalidArgument},
		{"", connect.CodeInvalidArgument},
	} {
		_, err := c.GetOperation(ctx, as("reader", &forgev1.GetOperationRequest{Name: tc.name}))
		if connect.CodeOf(err) != tc.code {
			t.Errorf("get %q: %v, want %v", tc.name, err, tc.code)
		}
	}

	if _, err := c.CancelOperation(ctx, as("reader", &forgev1.CancelOperationRequest{Name: op.GetName()})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader cancel: %v", err)
	}
	if _, err := c.GetOperation(ctx, connect.NewRequest(&forgev1.GetOperationRequest{Name: op.GetName()})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no token: %v", err)
	}

	stream, err := c.WatchOperation(ctx, as("reader", &forgev1.WatchOperationRequest{Name: op.GetName()}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() || stream.Msg().GetEvent().GetMessage() != "a" {
		t.Fatalf("first watch message: %v %v", stream.Msg(), stream.Err())
	}
	if _, err := c.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: op.GetName()})); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	var final *forgev1.Operation
	for stream.Receive() {
		final = stream.Msg().GetOperation()
	}
	if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if final.GetError().GetCode() != OpCodeCancelled {
		t.Errorf("final: %v", final)
	}
	again, err := c.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: op.GetName()}))
	if err != nil || again.Msg.GetOperation().GetError().GetCode() != OpCodeCancelled {
		t.Errorf("cancel again: %v %v", again, err)
	}
	if _, err := c.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: "operations/op_000000000000"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("cancel unknown: %v", err)
	}
}
