// Command cocoonbox-api serves the cocoonbox control plane and the agent edge.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/projecteru2/core/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/account"
	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/boxes"
	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/config"
	"github.com/loomhand/cocoonbox/api/connect"
	"github.com/loomhand/cocoonbox/api/console"
	"github.com/loomhand/cocoonbox/api/controlmcp"
	"github.com/loomhand/cocoonbox/api/credential"
	"github.com/loomhand/cocoonbox/api/dialer"
	"github.com/loomhand/cocoonbox/api/edge"
	"github.com/loomhand/cocoonbox/api/flows"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/machines/sandboxd"
	"github.com/loomhand/cocoonbox/api/oauth"
	"github.com/loomhand/cocoonbox/api/shares"
	"github.com/loomhand/cocoonbox/api/signup"
	"github.com/loomhand/cocoonbox/api/store/s3"
	"github.com/loomhand/cocoonbox/api/users"
	"github.com/loomhand/cocoonbox/api/vault"
)

const (
	provisionInterval   = 10 * time.Second
	credentialsInterval = time.Minute
	alarmInterval       = 15 * time.Second
	parkInterval        = time.Minute
	idleInterval        = time.Minute
	injectionTimeout    = time.Minute
	shutdownGrace       = 5 * time.Second
	verifyInterval      = time.Minute
	verifyTimeout       = 30 * time.Second
	readHeaderTimeout   = 10 * time.Second
	endShareTimeout     = 10 * time.Second
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
	if flag.NArg() > 0 {
		if err = runCLI(ctx, cfg, flag.Args(), os.Stdin, os.Stdout); err != nil {
			logger.Fatalf(ctx, err, "%s", strings.Join(flag.Args(), " "))
		}
		return
	}
	signals, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	var draining atomic.Bool
	ctx, cancel := drainAfter(signals, time.Duration(cfg.Cluster.Drain), &draining)
	defer cancel()

	flavors := configFlavors(cfg)
	pool := dialer.NewPool()
	defer pool.Close()
	var loops leaderLoops
	ledger, err := openLedger(ctx, cfg)
	if err != nil {
		logger.Fatalf(ctx, err, "open the cluster ledger")
	}
	defer ledger.Close()
	backend, err := newBackend(ctx, cfg, flavors, ledger)
	if err != nil {
		logger.Fatalf(ctx, err, "start sandboxd backend")
	}
	accounts, err := openUsersWith(ctx, cfg, backend)
	if err != nil {
		logger.Fatalf(ctx, err, "open users")
	}
	defer accounts.Close()
	if err = accounts.Reconcile(ctx); err != nil {
		logger.Error(ctx, err, "reconcile sandboxd tenants and gw caps")
	}
	go accounts.Listen(ctx)
	var broker *connect.Service
	if cfg.Connect.PublicURL != "" {
		if broker, err = openConnect(ctx, cfg, accounts); err != nil {
			logger.Fatalf(ctx, err, "open connect")
		}
		defer broker.Close()
		loops.add(broker.RunPurge)
		go broker.RunSignals(ctx)
	}
	svc := boxes.New(backend, boxes.Defaults{
		Flavor:       machineFlavor(cfg.Flavor),
		Flavors:      flavors,
		Image:        cfg.Image,
		RebaseImages: cfg.RebaseImages,
		MaxRunning:   cfg.MaxRunningBoxes,
	}, accounts)
	svc.EnableAccounts(accounts)
	svc.EnableUsage(accounts, cfg.Accounts.DefaultDailyCapMicros)
	svc.EnableWake(accounts, boxes.WakeLimits{
		MaxSchedules: cfg.Wake.MaxSchedules,
		Floor:        time.Duration(cfg.Wake.Floor),
		MaxAwakeFor:  time.Duration(cfg.Wake.MaxAwakeFor),
	})
	var grants *oauth.Service
	if cfg.OAuth.PublicURL != "" {
		if grants, err = openOAuth(ctx, cfg, accounts); err != nil {
			logger.Fatalf(ctx, err, "open oauth")
		}
		defer grants.Close()
	}
	enableIdle(ctx, cfg, svc, grants != nil, pool, &loops)
	var library *flows.Library
	if cfg.State.Bucket != "" {
		var st *s3.S3
		if st, err = newStore(cfg); err != nil {
			logger.Fatalf(ctx, err, "open the state store")
		}
		enableState(ctx, cfg, svc, dialer.New(pool), st, accounts, &loops)
		library = flows.NewLibrary(st, accounts)
	}
	enableLedger(ctx, cfg, ledger, backend, svc)
	signer, err := enableProvisioning(ctx, cfg, svc, accounts, &loops)
	if err != nil {
		logger.Fatalf(ctx, err, "enable provisioning")
	}
	keys, err := openVault(ctx, cfg, svc, accounts)
	if err != nil {
		logger.Fatalf(ctx, err, "open the vault")
	}
	defer keys.Close()
	proxy := edge.NewProxy(svc, signer, pool)
	accounts.OnShareEnd(func(ctx context.Context, sh users.Share) { go endShare(ctx, proxy, sh) })

	authn := auth.New(accounts, accounts)
	shareSvc := shares.New(accounts)
	access := boxes.NewAccessLog(authn.Identify)
	server := grpc.NewServer(
		grpc.ForceServerCodecV2(edge.NewCodec()),
		grpc.UnknownServiceHandler(proxy.Handle),
		grpc.ChainUnaryInterceptor(access.Unary, authn.Unary),
		grpc.ChainStreamInterceptor(access.Stream, authn.Stream),
		grpc.MaxRecvMsgSize(edge.MaxMessageSize),
	)
	controlv1.RegisterBoxesServer(server, svc)
	controlv1.RegisterSharesServer(server, shareSvc)
	signIns := account.New(accounts)
	controlv1.RegisterAccountServer(server, signIns)
	controlv1.RegisterKeysServer(server, keys)
	if library != nil {
		controlv1.RegisterFlowsServer(server, flows.NewServer(library))
	}

	httpMux := http.NewServeMux()
	httpMux.HandleFunc("GET /healthz", healthz(&draining))
	control := controlmcp.New(svc, authn)
	httpMux.Handle(controlmcp.Path, control)
	boxMCP := edge.NewMCP(svc, authn, signer)
	httpMux.Handle(edge.MCPPrefix, boxMCP)
	control.EnableBoxTools(boxMCP, accounts)
	httpMux.Handle(controlmcp.RootPath, control)
	web := console.New(accounts, svc, cfg.Accounts.DefaultDailyCapMicros, cfg.MaxRunningBoxes)
	web.EnableSharing(shareSvc, authn, proxy)
	web.EnableKeys(keys)
	web.EnableAccount(signIns)
	httpMux.Handle(console.Prefix, web)
	if broker != nil {
		controlv1.RegisterConnectorsServer(server, broker)
		web.EnableConnect(broker)
		httpMux.Handle(connect.MCPPath, broker.NewDoor(authn, connect.DoorMCP))
		httpMux.HandleFunc("GET "+connect.CallbackPath, web.ConnectCallback)
		httpMux.HandleFunc("GET "+connect.ClientMetadataPath, broker.ServeClientMetadata)
		enableBoxConnect(ctx, cfg, svc, broker, authn, library, &loops)
	}
	if grants != nil {
		control.EnableGrants(grants)
		signIns.EnableGrants(grants)
		boxMCP.EnableGrants(grants)
		web.EnableAgents(grants)
		httpMux.HandleFunc("GET "+grants.ResourceMetadataPath(), grants.ServeResourceMetadata)
		httpMux.HandleFunc("GET "+grants.ResourceMetadataPath()+"/", grants.ServeResourceMetadata)
		httpMux.HandleFunc("GET "+oauth.ServerMetadataPath, grants.ServeServerMetadata)
		httpMux.HandleFunc("POST "+oauth.TokenPath, grants.ServeToken)
		httpMux.HandleFunc("POST "+oauth.RegisterPath, grants.ServeRegister)
		httpMux.HandleFunc("POST "+oauth.RevokePath, grants.ServeRevoke)
		var sig *signup.Service
		if sig, err = openSignup(ctx, cfg, accounts); err != nil {
			logger.Fatalf(ctx, err, "open signup")
		}
		defer sig.Close()
		web.EnableSignup(sig)
		httpMux.HandleFunc("GET "+signup.CallbackPath, web.LoginCallback)
		loops.add(sig.Run)
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{
		Addr:              cfg.MCPListen,
		Handler:           console.InviteTokenGate(httpMux),
		Protocols:         &protocols,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	if cfg.OperatorSocket != "" {
		if err = serveOperator(ctx, cfg.OperatorSocket, svc); err != nil {
			logger.Fatalf(ctx, err, "serve the operator socket %s", cfg.OperatorSocket)
		}
	}
	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Fatalf(ctx, err, "listen on %s", cfg.Listen)
	}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf(ctx, err, "serve http on %s", cfg.MCPListen)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancelShutdown()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
		}
		timer := time.AfterFunc(shutdownGrace, server.Stop)
		server.GracefulStop()
		timer.Stop()
	}()
	loops.add(func(ctx context.Context) { svc.RunAlarms(ctx, alarmInterval) })
	if cfg.Cluster.Shared {
		loops.add(backend.Run)
	}
	var shared *cluster.Store
	if cfg.Cluster.Shared {
		shared = ledger
	}
	loops.run(ctx, shared)
	logger.Infof(ctx, "serving gRPC on %s and MCP and the console on %s for %d users", cfg.Listen, cfg.MCPListen, len(accounts.List()))
	if err := server.Serve(lis); err != nil {
		logger.Fatalf(ctx, err, "serve grpc")
	}
}

// drainAfter returns a context that ends drain after signals does, with draining set from the signal on.
func drainAfter(signals context.Context, drain time.Duration, draining *atomic.Bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(signals))
	context.AfterFunc(signals, func() {
		draining.Store(true)
		time.AfterFunc(drain, cancel)
	})
	return ctx, cancel
}

// healthz answers 503 while the api drains, so a balancer sends it no new calls.
func healthz(draining *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	}
}

// enableIdle hibernates idle boxes: an agent's after oauth.idle_hibernate while OAuth is on, a user's after user_idle_hibernate.
func enableIdle(ctx context.Context, cfg *config.Config, svc *boxes.Service, agents bool, pool *dialer.Pool, loops *leaderLoops) {
	agentIdle, userIdle := time.Duration(0), time.Duration(cfg.UserIdleHibernate)
	if agents {
		agentIdle = time.Duration(*cfg.OAuth.IdleHibernate)
	}
	if agentIdle == 0 && userIdle == 0 {
		return
	}
	svc.EnableIdle(agentIdle, userIdle)
	svc.EnableActivity(dialer.New(pool))
	loops.add(func(ctx context.Context) { svc.RunIdle(ctx, idleInterval) })
	log.WithFunc("main.enableIdle").Infof(ctx, "idle boxes hibernate: an agent's after %s, a user's after %s (0s: never)", agentIdle, userIdle)
}

func enableProvisioning(ctx context.Context, cfg *config.Config, svc *boxes.Service, accounts *users.Service, loops *leaderLoops) (*credential.Signer, error) {
	signer, err := credential.Load(cfg.Provision.SigningKeyFile)
	if err != nil {
		return nil, err
	}
	svc.EnableProvisioning(signer)
	if upstream := cfg.Provision.ResidentialUpstream; upstream != "" {
		svc.EnableResidential(upstream, func(_ context.Context, id string) (bool, error) { return accounts.Residential(id), nil })
		accounts.OnResidential(func(ctx context.Context, id string, _ bool) {
			if err := svc.ApplyExit(ctx, id); err != nil {
				log.WithFunc("main.enableProvisioning").Error(ctx, err, "apply residential exit")
			}
		})
	}
	loops.add(func(ctx context.Context) { svc.RunProvisioner(ctx, provisionInterval) })
	log.WithFunc("main.enableProvisioning").Infof(ctx, "provisioning every box with key %s, residential exit %q", signer.KeyID(), cfg.Provision.ResidentialUpstream)
	return signer, nil
}

// enableBoxConnect gives boxes their door keys and credentials and serves the box door on connect.box_listen.
func enableBoxConnect(ctx context.Context, cfg *config.Config, svc *boxes.Service, broker *connect.Service, authn *auth.Authenticator, library *flows.Library, loops *leaderLoops) {
	logger := log.WithFunc("main.enableBoxConnect")
	svc.EnableConnect(broker)
	if err := svc.PruneDoorKeys(ctx); err != nil {
		logger.Warnf(ctx, "prune the door keys of gone machines: %v", err)
	}
	broker.OnInjection(func(ctx context.Context, user string) {
		go func() {
			applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), injectionTimeout)
			defer cancel()
			if err := svc.ApplyCredentials(applyCtx, user); err != nil {
				logger.Warnf(applyCtx, "apply the credentials of %s: %v", user, err)
			}
		}()
	})
	loops.add(func(ctx context.Context) { svc.RunCredentials(ctx, credentialsInterval) })
	if cfg.Connect.BoxListen == "" {
		return
	}
	door := http.NewServeMux()
	door.Handle("/", broker.NewBoxDoor(authn))
	if library != nil {
		origin := func(ctx context.Context, _, machine string) flows.Origin {
			box, image := svc.MachineOrigin(ctx, machine)
			return flows.Origin{BoxID: box, Image: image}
		}
		flowsDoor := flows.NewDoor(library, broker.BoxCaller, origin)
		door.Handle(flows.DoorPath, flowsDoor)
		door.Handle(flows.DoorPath+"/", flowsDoor)
	}
	srv := &http.Server{Addr: cfg.Connect.BoxListen, Handler: console.InviteTokenGate(door), ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		logger.Infof(ctx, "serving the connect box door on %s", cfg.Connect.BoxListen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf(ctx, err, "serve the connect box door on %s", cfg.Connect.BoxListen)
		}
	}()
}

// openVault opens the key vault on the users database and puts each owner's keys on the owner's boxes, at once on a change.
func openVault(ctx context.Context, cfg *config.Config, svc *boxes.Service, accounts *users.Service) (*vault.Service, error) {
	dsn, err := cfg.DSN()
	if err != nil {
		return nil, err
	}
	keys, err := vault.Open(ctx, vault.Options{DSN: dsn, Keys: accounts})
	if err != nil {
		return nil, err
	}
	svc.EnableVault(keys)
	keys.OnChange(func(ctx context.Context, owner string) {
		go func() {
			applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), injectionTimeout)
			defer cancel()
			if err := svc.ApplyVault(applyCtx, owner); err != nil {
				log.WithFunc("main.openVault").Warnf(applyCtx, "apply the keys of %s: %v", owner, err)
			}
		}()
	})
	return keys, nil
}

// endShare closes the box's MCP sessions of a share that ended; a box that is not running has none to close.
func endShare(ctx context.Context, proxy *edge.Proxy, sh users.Share) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endShareTimeout)
	defer cancel()
	if err := proxy.EndShare(ctx, sh.Owner, sh.Box, sh.ID); err != nil {
		log.WithFunc("main.endShare").Infof(ctx, "end the sessions of share %s in box %s: %s", sh.ID, sh.Box, status.Convert(err).Message())
	}
}

// openConnect opens the Connect broker on the users database, sealing grants under each user's state key.
func openConnect(ctx context.Context, cfg *config.Config, accounts *users.Service) (*connect.Service, error) {
	dsn, err := cfg.DSN()
	if err != nil {
		return nil, err
	}
	broker, err := connect.Open(ctx, connect.Options{
		DSN:       dsn,
		Keys:      accounts,
		Sessions:  accounts,
		PublicURL: cfg.Connect.PublicURL,
		GitHub:    connect.Credentials{ClientID: os.Getenv("GITHUB_APP_CLIENT_ID"), ClientSecret: os.Getenv("GITHUB_APP_CLIENT_SECRET"), Slug: os.Getenv("GITHUB_APP_SLUG")},
		Google:    connect.Credentials{ClientID: os.Getenv("GOOGLE_CLIENT_ID"), ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET")},
		HubSpot:   connect.Credentials{ClientID: os.Getenv("HUBSPOT_CLIENT_ID"), ClientSecret: os.Getenv("HUBSPOT_CLIENT_SECRET")},
		Slack:     connect.Credentials{ClientID: os.Getenv("SLACK_CLIENT_ID"), ClientSecret: os.Getenv("SLACK_CLIENT_SECRET")},
		Link:      connect.Credentials{ClientID: os.Getenv("LINK_CLIENT_ID"), ClientSecret: os.Getenv("LINK_CLIENT_SECRET"), Key: os.Getenv("LINK_PUBLISHABLE_KEY")},
	})
	if err != nil {
		return nil, err
	}
	log.WithFunc("main.openConnect").Infof(ctx, "connect on %s", cfg.Connect.PublicURL)
	return broker, nil
}

// openSignup opens sign-in with GitHub and signup on the users database, with the login client from LOGIN_GITHUB_CLIENT_ID and _SECRET.
func openSignup(ctx context.Context, cfg *config.Config, accounts *users.Service) (*signup.Service, error) {
	dsn, err := cfg.DSN()
	if err != nil {
		return nil, err
	}
	sig, err := signup.Open(ctx, signup.Options{
		DSN: dsn, PublicURL: cfg.OAuth.PublicURL, Accounts: accounts,
		GitHub: signup.GitHubClient{ClientID: os.Getenv("LOGIN_GITHUB_CLIENT_ID"), ClientSecret: os.Getenv("LOGIN_GITHUB_CLIENT_SECRET")},
	})
	if err != nil {
		return nil, err
	}
	log.WithFunc("main.openSignup").Infof(ctx, "sign-in with GitHub configured: %t, callback %s%s", sig.Configured(), cfg.OAuth.PublicURL, signup.CallbackPath)
	return sig, nil
}

// openOAuth opens the authorization server of the control MCP on the users database.
func openOAuth(ctx context.Context, cfg *config.Config, accounts *users.Service) (*oauth.Service, error) {
	dsn, err := cfg.DSN()
	if err != nil {
		return nil, err
	}
	grants, err := oauth.Open(ctx, oauth.Options{DSN: dsn, Issuer: cfg.OAuth.PublicURL, Resource: cfg.OAuth.PublicURL + strings.TrimSuffix(edge.MCPPrefix, "/"), Accounts: accounts})
	if err != nil {
		return nil, err
	}
	accounts.KeepGrants(grants.LiveGrants)
	log.WithFunc("main.openOAuth").Infof(ctx, "oauth for the control MCP, issuer %s, agent boxes hibernate after %s idle", cfg.OAuth.PublicURL, time.Duration(*cfg.OAuth.IdleHibernate))
	return grants, nil
}

// openUsers opens the users table for operator commands, with a sandboxd client of its own for the tenants.
func openUsers(ctx context.Context, cfg *config.Config) (*users.Service, error) {
	token, err := cfg.SandboxdToken()
	if err != nil {
		return nil, err
	}
	tenants, err := sandboxd.NewTenants(cfg.Sandboxd.URL, token, cfg.Sandboxd.TenantEgressClass)
	if err != nil {
		return nil, err
	}
	return openUsersWith(ctx, cfg, tenants)
}

func openUsersWith(ctx context.Context, cfg *config.Config, tenants machines.Tenants) (*users.Service, error) {
	dsn, err := cfg.DSN()
	if err != nil {
		return nil, err
	}
	gwToken, err := cfg.GWAdminToken()
	if err != nil {
		return nil, err
	}
	gw, err := gwkeys.New(cfg.Provision.GWAdmin.URL, gwToken)
	if err != nil {
		return nil, err
	}
	return users.Open(ctx, users.Options{
		DSN:           dsn,
		MasterKeyFile: cfg.Accounts.MasterKeyFile,
		GW:            gw,
		Tenants:       tenants,
		TenantClaims:  cfg.Sandboxd.TenantEgressClass != "",
		FirstBoxID:    boxes.BoxID,
	})
}

func newBackend(ctx context.Context, cfg *config.Config, flavors []machines.Flavor, ledger *cluster.Store) (*sandboxd.Backend, error) {
	opts, err := backendOptions(cfg, flavors, ledger)
	if err != nil {
		return nil, err
	}
	return sandboxd.New(ctx, opts)
}

func backendOptions(cfg *config.Config, flavors []machines.Flavor, ledger *cluster.Store) (sandboxd.Options, error) {
	token, err := cfg.SandboxdToken()
	if err != nil {
		return sandboxd.Options{}, err
	}
	opts := sandboxd.Options{
		URL:               cfg.Sandboxd.URL,
		Token:             token,
		Image:             cfg.Image,
		Flavors:           append(flavors, machineFlavor(cfg.Flavor)),
		DiskGiB:           cfg.Flavor.DiskGiB,
		AgentPort:         cfg.AgentPort,
		TenantEgressClass: cfg.Sandboxd.TenantEgressClass,
	}
	if cfg.Cluster.Shared {
		opts.Shared = ledger
	} else {
		opts.Ledger = ledger
	}
	return opts, nil
}

func openLedger(ctx context.Context, cfg *config.Config) (*cluster.Store, error) {
	if !cfg.Cluster.Ledger {
		return nil, nil
	}
	dsn, err := cfg.DSN()
	if err != nil {
		return nil, err
	}
	return cluster.Open(ctx, dsn)
}

func enableLedger(ctx context.Context, cfg *config.Config, ledger *cluster.Store, backend *sandboxd.Backend, svc *boxes.Service) {
	if ledger == nil {
		return
	}
	svc.EnableLedger(ctx, ledger)
	if cfg.Cluster.Shared {
		svc.EnableShared(ctx, ledger)
		svc.SyncMachines(backend.SyncOwner)
	}
	go verifyLedger(ctx, backend, svc)
}

func verifyLedger(ctx context.Context, backend *sandboxd.Backend, svc *boxes.Service) {
	logger := log.WithFunc("main.verifyLedger")
	tick := time.Tick(verifyInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
		machineDrift, machineErr := backend.Verify(verifyCtx)
		drift, err := svc.Verify(verifyCtx)
		cancel()
		if err = errors.Join(machineErr, err); err != nil {
			logger.Error(ctx, err, "verify the ledger")
			continue
		}
		if n := len(machineDrift) + len(drift.Heads) + len(drift.Operations); n > 0 {
			logger.Warnf(ctx, "ledger drift repaired: machines %q, heads %q, operations %q", machineDrift, drift.Heads, drift.Operations)
			continue
		}
		logger.Debug(ctx, "ledger verified: no drift")
	}
}

func configFlavors(cfg *config.Config) []machines.Flavor {
	flavors := make([]machines.Flavor, 0, len(cfg.Flavors))
	for _, f := range cfg.Flavors {
		flavors = append(flavors, machineFlavor(f))
	}
	return flavors
}

func machineFlavor(f config.Flavor) machines.Flavor {
	return machines.Flavor{VCPUs: f.VCPUs, MemoryMiB: f.MemoryMiB, DiskGiB: f.DiskGiB}
}

func enableState(ctx context.Context, cfg *config.Config, svc *boxes.Service, agents *dialer.Dialer, st *s3.S3, accounts *users.Service, loops *leaderLoops) {
	svc.EnableState(agents, st, accounts)
	if cfg.State.ConditionalWrites {
		svc.EnableConditionalWrites(st)
	}
	loops.add(func(ctx context.Context) { svc.RunScheduler(ctx, time.Duration(cfg.State.Interval)) })
	logger := log.WithFunc("main.enableState")
	logger.Infof(ctx, "state layer on %s/%s, saves every %s, conditional writes %t", cfg.State.Endpoint, cfg.State.Bucket, time.Duration(cfg.State.Interval), cfg.State.ConditionalWrites)
	if after := time.Duration(cfg.State.ParkAfter); after > 0 {
		svc.EnableParking(after)
		loops.add(func(ctx context.Context) { svc.RunParking(ctx, parkInterval) })
		logger.Infof(ctx, "parking boxes hibernated for %s, except those of users set -no-park", after)
	}
	if gc := cfg.State.GC; gc.Interval > 0 {
		gcOpts := boxes.GCOptions{Interval: time.Duration(gc.Interval), DryRun: gc.DryRun, DeleteBatch: gc.DeleteBatch, DeletedRetention: time.Duration(gc.DeletedRetention)}
		loops.add(func(ctx context.Context) {
			svc.RunGC(ctx, gcOpts, func() []string {
				var ids []string
				for _, u := range accounts.List() {
					ids = append(ids, u.ID)
				}
				return ids
			})
		})
		logger.Infof(ctx, "state gc every %s, dry run %t, deleted boxes kept %s", time.Duration(gc.Interval), gc.DryRun, time.Duration(gc.DeletedRetention))
	}
}

func newStore(cfg *config.Config) (*s3.S3, error) {
	access, secret, err := cfg.StoreCredentials()
	if err != nil {
		return nil, err
	}
	return s3.New(s3.Options{
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
}
