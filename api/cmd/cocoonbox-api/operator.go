package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/config"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
)

// serveOperator serves boxes on the unix socket at path, each call as the user it names, until ctx ends.
func serveOperator(ctx context.Context, path string, boxes controlv1.BoxesServer) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the old operator socket: %w", err)
	}
	lis, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.Join(err, lis.Close())
	}
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(auth.OperatorUnary), grpc.ChainStreamInterceptor(auth.OperatorStream))
	controlv1.RegisterBoxesServer(server, boxes)
	context.AfterFunc(ctx, server.Stop)
	go func() {
		if err := server.Serve(lis); err != nil {
			log.WithFunc("main.serveOperator").Error(ctx, err, "serve the operator socket")
		}
	}()
	return nil
}

// rebaseBox rebases the box of a user, named or by id, to image through the running api and follows the operation to its end.
func rebaseBox(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) (err error) {
	if len(args) != 3 {
		return errors.New(cliUsage)
	}
	if cfg.OperatorSocket == "" {
		return errors.New("operator_socket is not configured")
	}
	accounts, err := openUsers(ctx, cfg)
	if err != nil {
		return err
	}
	defer accounts.Close()
	u, err := accounts.ByName(args[0])
	if err != nil {
		if u, err = accounts.Get(args[0]); err != nil {
			return err
		}
	}
	conn, err := grpc.NewClient("unix://"+cfg.OperatorSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	client := controlv1.NewBoxesClient(conn)
	ctx = metadata.AppendToOutgoingContext(ctx, auth.HeaderOperatorUser, u.ID)
	op, err := client.Rebase(ctx, &controlv1.RebaseRequest{BoxId: args[1], Image: args[2]})
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(stdout, "operation %s: rebase box %s of user %s (%s) to %s\n", op.GetId(), op.GetBoxId(), u.Name, u.ID, args[2]); err != nil {
		return err
	}
	return followOperation(ctx, client, op.GetId(), stdout)
}

// followOperation prints the events of operation id until it ends, and fails when it failed.
func followOperation(ctx context.Context, client controlv1.BoxesClient, id string, stdout io.Writer) error {
	stream, err := client.WatchOperation(ctx, &controlv1.WatchOperationRequest{OperationId: id})
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return errors.New("the operation stream ended before the operation did")
		}
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(stdout, "%s %s %s\n", ev.GetPhase(), ev.GetBoxState(), ev.GetDetail()); err != nil {
			return err
		}
		switch ev.GetPhase() {
		case controlv1.Phase_PHASE_DONE:
			return nil
		case controlv1.Phase_PHASE_FAILED:
			return fmt.Errorf("operation %s failed: %s", id, ev.GetDetail())
		}
	}
}
