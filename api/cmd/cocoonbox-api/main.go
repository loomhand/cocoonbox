// Command cocoonbox-api serves the cocoonbox control plane and the agent edge.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"net"
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
	"github.com/CMGS/cocoonbox/api/dialer"
	"github.com/CMGS/cocoonbox/api/edge"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/keyring/file"
	"github.com/CMGS/cocoonbox/api/machines"
	"github.com/CMGS/cocoonbox/api/machines/cocoonset"
	"github.com/CMGS/cocoonbox/api/store/s3"
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
	svc := boxes.New(backend, boxes.Defaults{
		Flavor:       machines.Flavor{VCPUs: cfg.Flavor.VCPUs, MemoryMiB: cfg.Flavor.MemoryMiB, DiskGiB: cfg.Flavor.DiskGiB},
		Image:        cfg.Image,
		RebaseImages: cfg.RebaseImages,
	})
	if cfg.State.Bucket != "" {
		agents := dialer.New(edgeToken)
		defer agents.Close()
		if err = enableState(ctx, cfg, svc, agents); err != nil {
			logger.Fatalf(ctx, err, "enable state layer")
		}
	}
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
	mcpServer := &http.Server{
		Addr:              cfg.MCPListen,
		Handler:           edge.NewMCP(svc, authn, edgeToken),
		Protocols:         &protocols,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Fatalf(ctx, err, "listen on %s", cfg.Listen)
	}
	go func() {
		if err := mcpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf(ctx, err, "serve mcp on %s", cfg.MCPListen)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancelShutdown()
		if err := mcpServer.Shutdown(shutdownCtx); err != nil {
			_ = mcpServer.Close()
		}
		timer := time.AfterFunc(shutdownGrace, server.Stop)
		server.GracefulStop()
		timer.Stop()
	}()
	logger.Infof(ctx, "serving gRPC on %s and MCP on %s for %d users, namespace %s", cfg.Listen, cfg.MCPListen, len(cfg.Users), cfg.Namespace)
	if err := server.Serve(lis); err != nil {
		logger.Fatalf(ctx, err, "serve grpc")
	}
}

func enableState(ctx context.Context, cfg *config.Config, svc *boxes.Service, agents *dialer.Dialer) error {
	access, secret, err := cfg.StoreCredentials()
	if err != nil {
		return err
	}
	st, err := s3.New(s3.Options{
		Endpoint:       cfg.State.Endpoint,
		PublicEndpoint: cfg.State.PublicEndpoint,
		Secure:         cfg.State.Secure,
		Region:         cfg.State.Region,
		Bucket:         cfg.State.Bucket,
		Prefix:         cfg.State.Prefix,
		AccessKey:      access,
		SecretKey:      secret,
		PresignTTL:     time.Duration(cfg.State.PresignTTL),
	})
	if err != nil {
		return err
	}
	keys, err := file.New(cfg.State.KeyringDir)
	if err != nil {
		return err
	}
	svc.EnableState(agents, st, keys)
	go svc.RunScheduler(ctx, time.Duration(cfg.State.Interval), cfg.State.ScheduledUsers)
	log.WithFunc("main.enableState").Infof(ctx, "state layer on %s/%s, saves every %s", cfg.State.Endpoint, cfg.State.Bucket, time.Duration(cfg.State.Interval))
	return nil
}
