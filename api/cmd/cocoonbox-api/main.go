// Command cocoonbox-api serves the cocoonbox control plane and the agent edge.
package main

import (
	"cmp"
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/projecteru2/core/types"
	"google.golang.org/grpc"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/boxes"
	"github.com/CMGS/cocoonbox/api/config"
	"github.com/CMGS/cocoonbox/api/edge"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const shutdownGrace = 5 * time.Second

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
		logger.Fatalf(ctx, err, "load edge token")
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", cmp.Or(cfg.Kubeconfig, os.Getenv("KUBECONFIG")))
	if err != nil {
		logger.Fatalf(ctx, err, "load kubeconfig")
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		logger.Fatalf(ctx, err, "build dynamic client")
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Fatalf(ctx, err, "build clientset")
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	svc, err := boxes.New(ctx, cfg, dyn, kube)
	if err != nil {
		logger.Fatalf(ctx, err, "start boxes service")
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

	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Fatalf(ctx, err, "listen on %s", cfg.Listen)
	}
	go func() {
		<-ctx.Done()
		timer := time.AfterFunc(shutdownGrace, server.Stop)
		server.GracefulStop()
		timer.Stop()
	}()
	logger.Infof(ctx, "serving on %s for %d users, namespace %s", cfg.Listen, len(cfg.Users), cfg.Namespace)
	if err := server.Serve(lis); err != nil {
		logger.Fatalf(ctx, err, "serve")
	}
}
