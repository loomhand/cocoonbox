package boxes

import (
	"context"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

const boxesServicePrefix = "/cocoonbox.control.v1.Boxes/"

type AccessLog struct {
	identify func(context.Context) string
}

func NewAccessLog(identify func(context.Context) string) *AccessLog {
	return &AccessLog{identify: identify}
}

func (a *AccessLog) Unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if !strings.HasPrefix(info.FullMethod, boxesServicePrefix) {
		return handler(ctx, req)
	}
	start := time.Now()
	resp, err := handler(ctx, req)
	a.write(ctx, info.FullMethod, boxOf(req), err, start)
	return resp, err
}

func (a *AccessLog) Stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if !strings.HasPrefix(info.FullMethod, boxesServicePrefix) {
		return handler(srv, ss)
	}
	start := time.Now()
	err := handler(srv, ss)
	a.write(ss.Context(), info.FullMethod, "", err, start)
	return err
}

func (a *AccessLog) write(ctx context.Context, method, box string, err error, start time.Time) {
	log.WithFunc("boxes.AccessLog").Infof(ctx, "%s user=%s box=%q code=%s in %s",
		strings.TrimPrefix(method, boxesServicePrefix), a.identify(ctx), box, status.Code(err), time.Since(start).Round(time.Millisecond))
}

type boxRequest interface {
	GetBoxId() string
}

func boxOf(req any) string {
	if r, ok := req.(boxRequest); ok {
		return r.GetBoxId()
	}
	return ""
}
