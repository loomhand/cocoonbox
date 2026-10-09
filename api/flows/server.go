package flows

import (
	"context"
	"errors"
	"fmt"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

// Server implements controlv1.FlowsServer on the authenticated caller's own library.
type Server struct {
	controlv1.UnimplementedFlowsServer

	lib *Library
}

// NewServer serves lib to its owners.
func NewServer(lib *Library) *Server {
	return &Server{lib: lib}
}

func (s *Server) List(ctx context.Context, _ *controlv1.ListFlowsRequest) (*controlv1.ListFlowsResponse, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	rev, entries, err := s.lib.List(ctx, user)
	if err != nil {
		return nil, libraryError(ctx, err, "list flows")
	}
	resp := &controlv1.ListFlowsResponse{Rev: rev}
	for _, e := range entries {
		resp.Flows = append(resp.Flows, toFlow(e, nil))
	}
	return resp, nil
}

func (s *Server) Export(ctx context.Context, req *controlv1.ExportFlowsRequest) (*controlv1.ExportFlowsResponse, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	names := req.GetNames()
	if len(names) == 0 {
		_, entries, err := s.lib.List(ctx, user)
		if err != nil {
			return nil, libraryError(ctx, err, "list flows")
		}
		for _, e := range entries {
			names = append(names, e.Name)
		}
	}
	resp := &controlv1.ExportFlowsResponse{}
	for _, name := range names {
		e, content, err := s.lib.Get(ctx, user, name)
		if err != nil {
			return nil, libraryError(ctx, fmt.Errorf("%s: %w", name, err), "export flow")
		}
		resp.Flows = append(resp.Flows, toFlow(e, content))
	}
	return resp, nil
}

func (s *Server) Save(ctx context.Context, req *controlv1.SaveFlowsRequest) (*controlv1.SaveFlowsResponse, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range req.GetFlows() {
		if err := Check(f.GetName(), f.GetContent()); err != nil {
			return nil, libraryError(ctx, err, "import flows")
		}
	}
	resp := &controlv1.SaveFlowsResponse{}
	for _, f := range req.GetFlows() {
		e, err := s.lib.Put(ctx, user, f.GetName(), f.GetContent(), Origin{Image: f.GetImage()})
		if err != nil {
			return nil, libraryError(ctx, fmt.Errorf("%s: %w", f.GetName(), err), "import flow")
		}
		resp.Flows = append(resp.Flows, toFlow(e, nil))
	}
	log.WithFunc("flows.Server.Save").Infof(ctx, "user %s imported %d flows", user, len(resp.Flows))
	return resp, nil
}

func (s *Server) Delete(ctx context.Context, req *controlv1.DeleteFlowsRequest) (*controlv1.DeleteFlowsResponse, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	deleted, err := s.lib.Delete(ctx, user, req.GetNames())
	if err != nil {
		return nil, libraryError(ctx, err, "delete flows")
	}
	return &controlv1.DeleteFlowsResponse{Deleted: deleted}, nil
}

func caller(ctx context.Context) (string, error) {
	user := auth.User(ctx)
	if user == "" {
		return "", status.Error(codes.Unauthenticated, "no user on the call")
	}
	return user, nil
}

func libraryError(ctx context.Context, err error, what string) error {
	switch {
	case errors.Is(err, ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrFull):
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	log.WithFunc("flows.libraryError").Error(ctx, err, what)
	return status.Error(codes.Internal, what+" failed")
}

func toFlow(e Entry, content []byte) *controlv1.Flow {
	return &controlv1.Flow{
		Name: e.Name, Content: content, Rev: e.Rev, Sha256: e.SHA256, Size: uint64(e.Size), //nolint:gosec // a size is never negative
		SavedAtMs: e.SavedAtMs, BoxId: e.BoxID, Image: e.Image,
	}
}
