package dialer

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"

	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	offloadStatus   = "cocoonbox-agent offload status"
	commandNotFound = 127
	maxStatusOutput = 1 << 20
)

type offloadSession struct {
	Status string `json:"status"`
}

type offloadReply struct {
	Sessions []offloadSession `json:"sessions"`
}

// Busy reports whether work runs in the box at ep: an agent turn, or an offloaded CLI session at work. ctx carries the
// owner's credential; a box whose image has no offload command has no offloaded session.
func (d *Dialer) Busy(ctx context.Context, box string, ep machines.Endpoint) (bool, error) {
	conn, err := d.pool.Conn(box, ep)
	if err != nil {
		return false, fmt.Errorf("dial agent %s: %w", ep.Key, err)
	}
	list, err := agentv1.NewAgentsClient(conn).List(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		return false, fmt.Errorf("list agents: %w", err)
	}
	if slices.ContainsFunc(list.GetAgents(), func(a *agentv1.Agent) bool { return a.GetStatus() == agentv1.AgentStatus_AGENT_STATUS_RUNNING }) {
		return true, nil
	}
	out, code, err := run(ctx, agentv1.NewSessionClient(conn), offloadStatus)
	switch {
	case err != nil:
		return false, fmt.Errorf("offload status: %w", err)
	case code == commandNotFound:
		return false, nil
	case code != 0:
		return false, fmt.Errorf("offload status exited %d", code)
	}
	var status offloadReply
	if err := json.Unmarshal(out, &status); err != nil {
		return false, fmt.Errorf("offload status: %w", err)
	}
	return slices.ContainsFunc(status.Sessions, func(s offloadSession) bool { return s.Status == "working" }), nil
}

// run runs command by Session.Exec with stdin closed and returns its stdout and exit code.
func run(ctx context.Context, session agentv1.SessionClient, command string) ([]byte, int32, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := session.Exec(ctx)
	if err != nil {
		return nil, 0, err
	}
	if err := stream.Send(&agentv1.ExecClientFrame{Body: &agentv1.ExecClientFrame_Start{Start: &agentv1.ExecStart{Command: command}}}); err != nil {
		return nil, 0, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, 0, err
	}
	var out []byte
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, 0, errors.New("the agent ended Exec without an exit code")
		}
		if err != nil {
			return nil, 0, err
		}
		switch body := frame.GetBody().(type) {
		case *agentv1.ExecServerFrame_Stdout:
			if len(out)+len(body.Stdout) > maxStatusOutput {
				return nil, 0, fmt.Errorf("output past %d bytes", maxStatusOutput)
			}
			out = append(out, body.Stdout...)
		case *agentv1.ExecServerFrame_ExitCode:
			return out, body.ExitCode, nil
		}
	}
}
