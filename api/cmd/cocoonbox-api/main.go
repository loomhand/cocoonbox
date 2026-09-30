// Command cocoonbox-api serves the cocoonbox control plane and the agent edge.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/projecteru2/core/types"
	"google.golang.org/grpc"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/boxes"
	"github.com/CMGS/cocoonbox/api/config"
	"github.com/CMGS/cocoonbox/api/edge"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
	"github.com/CMGS/cocoonbox/api/machines/cocoonset"
)

const (
	shutdownGrace     = 5 * time.Second
	readHeaderTimeout = 10 * time.Second
)

func main() {
	configPath := flag.String("config", "/etc/cocoonbox/api.yaml", "config file")
	flag.Parse()

	ctx := context.Background()
	logger := log.WithFunc("main")
	if err := log.SetupLog(ctx, &types.ServerLogConfig{Level: cmp.Or(os.Getenv("COCOONBOX_LOG_LEVEL"), "info")}, ""); err != nil {
		logger.Fatalf(ctx, err, "setup log")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf(ctx, err, "load config")
	}
	edgeToken, err := cfg.EdgeToken()
	if err != nil {
		logger.Warnf(ctx, "agent edge refuses every call until the edge token exists and the service restarts: %v", err)
	}
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	backend, err := cocoonset.New(ctx, cocoonset.Options{
		Kubeconfig: cfg.Kubeconfig,
		Namespace:  cfg.Namespace,
		Image:      cfg.Image,
		Network:    cfg.Network,
		ProbePort:  cfg.ProbePort,
		AgentPort:  cfg.AgentPort,
	})
	if err != nil {
		logger.Fatalf(ctx, err, "start cocoonset backend")
	}
	svc := boxes.New(backend, machines.Flavor{VCPUs: cfg.Flavor.VCPUs, MemoryMiB: cfg.Flavor.MemoryMiB, DiskGiB: cfg.Flavor.DiskGiB})
	proxy := edge.NewProxy(svc, edgeToken)
	defer proxy.Close()

	authn := auth.New(cfg.Users)
	server := grpc.NewServer(
		grpc.ForceServerCodecV2(edge.NewCodec()),
		grpc.UnknownServiceHandler(proxy.Handle),
		grpc.ChainUnaryInterceptor(authn.Unary),
		grpc.ChainStreamInterceptor(authn.Stream),
		grpc.MaxRecvMsgSize(edge.MaxMessageSize),
	)
	controlv1.RegisterBoxesServer(server, svc)

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           edge.NewHandler(server, edge.NewMCP(svc, authn, edgeToken)),
		Protocols:         &protocols,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancelShutdown()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
		}
	}()
	logger.Infof(ctx, "serving gRPC and MCP on %s for %d users, namespace %s", cfg.Listen, len(cfg.Users), cfg.Namespace)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf(ctx, err, "serve")
	}
}
