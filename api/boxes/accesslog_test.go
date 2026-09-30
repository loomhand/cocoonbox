package boxes

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

func TestAccessLogCoversOnlyBoxes(t *testing.T) {
	var identified []string
	a := NewAccessLog(func(context.Context) string { identified = append(identified, "x"); return "alice" })
	denied := func(context.Context, any) (any, error) { return nil, status.Error(codes.PermissionDenied, "no") }

	_, err := a.Unary(t.Context(), &controlv1.BoxRef{BoxId: aliceBox}, &grpc.UnaryServerInfo{FullMethod: "/cocoonbox.control.v1.Boxes/Hibernate"}, denied)
	if status.Code(err) != codes.PermissionDenied || len(identified) != 1 {
		t.Errorf("Boxes call: %v, identified %d times", err, len(identified))
	}
	if boxOf(&controlv1.BoxRef{BoxId: aliceBox}) != aliceBox || boxOf(&controlv1.ListBoxesRequest{}) != "" {
		t.Error("boxOf does not read box ids")
	}
	_, _ = a.Unary(t.Context(), nil, &grpc.UnaryServerInfo{FullMethod: "/cocoonbox.agent.v1.Session/Hello"}, denied)
	err = a.Stream(nil, &eventStream{ctx: t.Context()}, &grpc.StreamServerInfo{FullMethod: "/cocoonbox.agent.v1.Session/Screen"},
		func(any, grpc.ServerStream) error { return nil })
	if err != nil || len(identified) != 1 {
		t.Errorf("agent calls were logged: identified %d times, err %v", len(identified), err)
	}
}
