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
	"github.com/CMGS/cocoonbox/api/controlmcp"
	"github.com/CMGS/cocoonbox/api/credential"
	"github.com/CMGS/cocoonbox/api/dialer"
	"github.com/CMGS/cocoonbox/api/edge"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/gwkeys"
	"github.com/CMGS/cocoonbox/api/keyring/file"
	"github.com/CMGS/cocoonbox/api/machines"
	"github.com/CMGS/cocoonbox/api/machines/sandboxd"
	"github.com/CMGS/cocoonbox/api/store/s3"
)

const (
	provisionInterval = 10 * time.Second
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
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	flavors := make([]machines.Flavor, 0, len(cfg.Flavors))
	for _, f := range cfg.Flavors {
		flavors = append(flavors, machineFlavor(f))
	}
	backend, err := newBackend(ctx, cfg, flavors)
	if err != nil {
		logger.Fatalf(ctx, err, "start sandboxd backend")
	}
	svc := boxes.New(backend, boxes.Defaults{
		Flavor:       machineFlavor(cfg.Flavor),
		Flavors:      flavors,
		Image:        cfg.Image,
		RebaseImages: cfg.RebaseImages,
	})
	if cfg.State.Bucket != "" {
		agents := dialer.New()
		defer agents.Close()
		if err = enableState(ctx, cfg, svc, agents); err != nil {
			logger.Fatalf(ctx, err, "enable state layer")
		}
	}
	signer, err := enableProvisioning(ctx, cfg, svc)
	if err != nil {
		logger.Fatalf(ctx, err, "enable provisioning")
	}
	proxy := edge.NewProxy(svc, signer)
	defer proxy.Close()

	authn := auth.New(cfg.Users)
	access := boxes.NewAccessLog(authn.Identify)
	server := grpc.NewServer(
		grpc.ForceServerCodecV2(edge.NewCodec()),
		grpc.UnknownServiceHandler(proxy.Handle),
		grpc.ChainUnaryInterceptor(access.Unary, authn.Unary),
		grpc.ChainStreamInterceptor(access.Stream, authn.Stream),
		grpc.MaxRecvMsgSize(edge.MaxMessageSize),
	)
	controlv1.RegisterBoxesServer(server, svc)

	mcpMux := http.NewServeMux()
	mcpMux.Handle(controlmcp.Path, controlmcp.New(svc, authn))
	mcpMux.Handle(edge.MCPPrefix, edge.NewMCP(svc, authn, signer))
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	mcpServer := &http.Server{
		Addr:              cfg.MCPListen,
		Handler:           mcpMux,
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
	logger.Infof(ctx, "serving gRPC on %s and MCP on %s for %d users", cfg.Listen, cfg.MCPListen, len(cfg.Users))
	if err := server.Serve(lis); err != nil {
		logger.Fatalf(ctx, err, "serve grpc")
	}
}

func enableProvisioning(ctx context.Context, cfg *config.Config, svc *boxes.Service) (*credential.Signer, error) {
	signer, err := credential.Load(cfg.Provision.SigningKeyFile)
	if err != nil {
		return nil, err
	}
	var gw boxes.GWKeys
	if cfg.Provision.GWAdmin.URL != "" {
		token, err := cfg.GWAdminToken()
		if err != nil {
			return nil, err
		}
		if gw, err = gwkeys.New(cfg.Provision.GWAdmin.URL, token); err != nil {
			return nil, err
		}
	}
	svc.EnableProvisioning(signer, gw)
	go svc.RunProvisioner(ctx, provisionInterval)
	log.WithFunc("main.enableProvisioning").Infof(ctx, "provisioning every box with key %s, gw keys %t", signer.KeyID(), gw != nil)
	return signer, nil
}

func newBackend(ctx context.Context, cfg *config.Config, flavors []machines.Flavor) (machines.Machines, error) {
	token, err := cfg.SandboxdToken()
	if err != nil {
		return nil, err
	}
	return sandboxd.New(ctx, sandboxd.Options{
		URL:       cfg.Sandboxd.URL,
		Token:     token,
		Image:     cfg.Image,
		Flavors:   append(flavors, machineFlavor(cfg.Flavor)),
		DiskGiB:   cfg.Flavor.DiskGiB,
		AgentPort: cfg.AgentPort,
	})
}

func machineFlavor(f config.Flavor) machines.Flavor {
	return machines.Flavor{VCPUs: f.VCPUs, MemoryMiB: f.MemoryMiB, DiskGiB: f.DiskGiB}
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
