package edge

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc/metadata"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/credential"
	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
)

const viewerReadChunk = 32 << 10

// Screen bridges a share's viewer to the box's Session.Screen until either side closes or the share ends;
// the viewer's input never reaches the box.
func (p *Proxy) Screen(ctx context.Context, g auth.Grant, viewer io.ReadWriter) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-g.Cut:
		case <-ctx.Done():
		}
		cancel()
	}()
	ep, wake, err := p.resolver.Resolve(ctx, g.Owner, g.Box)
	if err != nil {
		return err
	}
	conn, err := p.conn(ctx, g.Box, ep)
	if err != nil {
		return err
	}
	if wake != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, credential.Header, p.creds.ShareCredential(g.Box, wake, g.Principal()))
	}
	stream, err := agentv1.NewSessionClient(conn).Screen(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&agentv1.ScreenClientFrame{Body: &agentv1.ScreenClientFrame_Open{Open: &agentv1.ScreenOpen{}}}); err != nil {
		return err
	}
	go func() {
		forwardViewer(stream, viewer)
		cancel()
	}()
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := viewer.Write(frame.GetData()); err != nil {
			return err
		}
	}
}

// EndShare asks the box agent to close every MCP session of share id.
func (p *Proxy) EndShare(ctx context.Context, owner, box, id string) error {
	ep, wake, err := p.resolver.Resolve(ctx, owner, box)
	if err != nil {
		return err
	}
	conn, err := p.conn(ctx, box, ep)
	if err != nil {
		return err
	}
	if wake != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, credential.Header, p.creds.Credential(box, wake))
	}
	_, err = agentv1.NewAgentsClient(conn).End(ctx, &agentv1.EndRequest{Target: &agentv1.EndRequest_ShareId{ShareId: id}})
	return err
}

// forwardViewer sends the viewer's handshake and framebuffer requests to the box until the viewer closes or sends something else.
func forwardViewer(stream agentv1.Session_ScreenClient, viewer io.Reader) {
	var filter viewOnly
	buf := make([]byte, viewerReadChunk)
	for {
		n, err := viewer.Read(buf)
		if n > 0 {
			out, ferr := filter.feed(buf[:n])
			if ferr != nil {
				return
			}
			if len(out) > 0 && stream.Send(&agentv1.ScreenClientFrame{Body: &agentv1.ScreenClientFrame_Data{Data: out}}) != nil {
				return
			}
		}
		if err != nil {
			_ = stream.CloseSend()
			return
		}
	}
}
