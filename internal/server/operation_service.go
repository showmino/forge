package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

var opNameRE = regexp.MustCompile(`^operations/op_[0-9a-f]{12}$`)

// validOpName checks an "operations/op_<12 hex>" name.
func validOpName(name string) error {
	if !opNameRE.MatchString(name) {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q is not an operation name (operations/op_<id>)", name))
	}
	return nil
}

func opNotFound(name string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", name))
}

type operationService struct{ ops *Operations }

func (s *operationService) GetOperation(ctx context.Context, req *connect.Request[forgev1.GetOperationRequest]) (*connect.Response[forgev1.GetOperationResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if err := validOpName(name); err != nil {
		return nil, err
	}
	op, ok := s.ops.Get(name)
	if !ok {
		return nil, opNotFound(name)
	}
	return connect.NewResponse(&forgev1.GetOperationResponse{Operation: op}), nil
}

func (s *operationService) ListOperations(ctx context.Context, req *connect.Request[forgev1.ListOperationsRequest]) (*connect.Response[forgev1.ListOperationsResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.ListOperationsResponse{
		Operations: s.ops.List(req.Msg.GetTarget(), req.Msg.GetRunning()),
	}), nil
}

func (s *operationService) WatchOperation(ctx context.Context, req *connect.Request[forgev1.WatchOperationRequest], stream *connect.ServerStream[forgev1.WatchOperationResponse]) error {
	if err := require(ctx, PermServicesRead); err != nil {
		return err
	}
	name := req.Msg.GetName()
	if err := validOpName(name); err != nil {
		return err
	}
	err := s.ops.Watch(ctx, name, stream.Send)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil // the client went away; nothing to report to it
	}
	return err
}

func (s *operationService) CancelOperation(ctx context.Context, req *connect.Request[forgev1.CancelOperationRequest]) (*connect.Response[forgev1.CancelOperationResponse], error) {
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if err := validOpName(name); err != nil {
		return nil, err
	}
	op, ok := s.ops.Cancel(name)
	if !ok {
		return nil, opNotFound(name)
	}
	return connect.NewResponse(&forgev1.CancelOperationResponse{Operation: op}), nil
}
