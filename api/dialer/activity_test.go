package dialer

import (
	"context"
	"io"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/loomhand/cocoonbox/api/credential"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

func TestBusyAsksForAgentTurnsThenOffloadedSessions(t *testing.T) {
	tests := []struct {
		name    string
		agent   agentv1.AgentStatus
		code    int32
		stdout  string
		want    bool
		wantErr bool
	}{
		{name: "a turn runs", agent: agentv1.AgentStatus_AGENT_STATUS_RUNNING, want: true},
		{name: "an offloaded session works", agent: agentv1.AgentStatus_AGENT_STATUS_IDLE, stdout: `{"ok":true,"sessions":[{"status":"idle"},{"status":"working"}]}`, want: true},
		{name: "nothing works", agent: agentv1.AgentStatus_AGENT_STATUS_WAITING_FOR_USER, stdout: `{"ok":true,"sessions":[{"status":"waiting_for_user"}]}`},
		{name: "an image without offload", agent: agentv1.AgentStatus_AGENT_STATUS_IDLE, code: commandNotFound},
		{name: "offload status fails", agent: agentv1.AgentStatus_AGENT_STATUS_IDLE, code: 1, wantErr: true},
		{name: "offload status answers no JSON", agent: agentv1.AgentStatus_AGENT_STATUS_IDLE, stdout: "oops", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &fakeBox{status: tt.agent, code: tt.code, stdout: tt.stdout}
			ep := machines.TCPEndpoint(serve(t, func(s *grpc.Server) {
				agentv1.RegisterAgentsServer(s, box)
				agentv1.RegisterSessionServer(s, box)
			}))
			busy, err := newDialer(t).Busy(credCtx(t), "cb-1", ep)
			if busy != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("Busy = %t, %v; want %t, error %t", busy, err, tt.want, tt.wantErr)
			}
			if box.credential != "cred-1" {
				t.Errorf("the agent saw credential %q", box.credential)
			}
			if tt.agent != agentv1.AgentStatus_AGENT_STATUS_RUNNING && box.command != offloadStatus {
				t.Errorf("Exec ran %q, want %q", box.command, offloadStatus)
			}
		})
	}
}

type fakeBox struct {
	agentv1.UnimplementedAgentsServer
	agentv1.UnimplementedSessionServer

	status     agentv1.AgentStatus
	code       int32
	stdout     string
	credential string
	command    string
}

func (b *fakeBox) List(ctx context.Context, _ *agentv1.ListAgentsRequest) (*agentv1.ListAgentsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(credential.Header); len(v) == 1 {
		b.credential = v[0]
	}
	return &agentv1.ListAgentsResponse{Agents: []*agentv1.Agent{{Id: "a1", Status: b.status}}}, nil
}

func (b *fakeBox) Exec(stream grpc.BidiStreamingServer[agentv1.ExecClientFrame, agentv1.ExecServerFrame]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	b.command = first.GetStart().GetCommand()
	if _, err := stream.Recv(); err != io.EOF {
		return err
	}
	if b.stdout != "" {
		if err := stream.Send(&agentv1.ExecServerFrame{Body: &agentv1.ExecServerFrame_Stdout{Stdout: []byte(b.stdout)}}); err != nil {
			return err
		}
	}
	return stream.Send(&agentv1.ExecServerFrame{Body: &agentv1.ExecServerFrame_ExitCode{ExitCode: b.code}})
}
