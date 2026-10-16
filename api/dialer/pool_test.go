package dialer

import (
	"testing"
	"time"

	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"

	"github.com/loomhand/cocoonbox/api/credential"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

func TestPoolKeepsOneConnectionPerEndpointAndClosesTheOnesABoxLeftIdle(t *testing.T) {
	pool := NewPool()
	t.Cleanup(pool.Close)
	clock := time.Unix(1_000_000, 0)
	pool.now = func() time.Time { return clock }
	first, err := pool.Conn("cb-1", machines.TCPEndpoint("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	if again, _ := pool.Conn("cb-1", machines.TCPEndpoint("127.0.0.1:1")); again != first {
		t.Error("the same box and endpoint got a second connection")
	}
	other, _ := pool.Conn("cb-2", machines.TCPEndpoint("127.0.0.1:1"))
	if other == first {
		t.Error("two boxes share a connection")
	}
	moved, _ := pool.Conn("cb-1", machines.TCPEndpoint("127.0.0.1:2"))
	if moved == first || first.GetState() == connectivity.Shutdown || len(pool.conns["cb-1"]) != 2 {
		t.Errorf("a connection used within the idle timeout closed: state %s, %d held", first.GetState(), len(pool.conns["cb-1"]))
	}
	clock = clock.Add(idleTimeout + time.Second)
	if _, err := pool.Conn("cb-1", machines.TCPEndpoint("127.0.0.1:3")); err != nil {
		t.Fatalf("Conn: %v", err)
	}
	if first.GetState() != connectivity.Shutdown || moved.GetState() != connectivity.Shutdown || len(pool.conns["cb-1"]) != 1 {
		t.Errorf("idle connections of endpoints the box left: %s %s, %d held", first.GetState(), moved.GetState(), len(pool.conns["cb-1"]))
	}
	pool.Close()
	if other.GetState() != connectivity.Shutdown || len(pool.conns) != 0 {
		t.Errorf("after Close: %s, %d boxes left", other.GetState(), len(pool.conns))
	}
}

func TestRebaseRestoreSurvivesAnEdgeCallToTheOldMachine(t *testing.T) {
	oldEp := machines.TCPEndpoint(serveState(t, &fakeState{}))
	newEp := machines.TCPEndpoint(serveState(t, &fakeState{}))
	pool := NewPool()
	t.Cleanup(pool.Close)
	clock := time.Unix(1_000_000, 0)
	pool.now = func() time.Time { return clock }
	d := New(pool)
	ctx := metadata.AppendToOutgoingContext(t.Context(), credential.Header, "cred-1")
	midway := make(chan struct{})
	resume := make(chan struct{})
	reqs := func(yield func(*agentv1.RestoreRequest, error) bool) {
		if !yield(&agentv1.RestoreRequest{Manifest: &agentv1.PresignedGet{ChunkId: "m"}}, nil) {
			return
		}
		close(midway)
		<-resume
		yield(&agentv1.RestoreRequest{Gets: []*agentv1.PresignedGet{{ChunkId: "a"}}}, nil)
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.Restore(ctx, "cb-1", newEp, reqs, nil)
		done <- err
	}()
	<-midway
	clock = clock.Add(idleTimeout + time.Second)
	if _, err := pool.Conn("cb-1", oldEp); err != nil {
		t.Fatalf("edge Conn: %v", err)
	}
	close(resume)
	if err := <-done; err != nil {
		t.Errorf("restore into the new machine failed after an edge call to the old one: %v", err)
	}
}
