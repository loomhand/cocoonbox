package boxes

import (
	"encoding/json/v2"
	"slices"
	"testing"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
)

func TestTheLedgerKeepsHeadsAndOperationsAndVerifyRepairsThem(t *testing.T) {
	ledger := clustertest.Open(t)
	env := newStateEnv(t)
	env.svc.EnableLedger(t.Context(), ledger)
	env.agent.chunks = hexIDs("c1")
	events := env.save(t)
	if d, err := env.svc.Verify(t.Context()); err != nil || len(d.Heads)+len(d.Operations) != 0 {
		t.Fatalf("Verify after a save = %+v, %v, want no drift", d, err)
	}
	row, err := ledger.Head(t.Context(), alice, aliceBox)
	var h head
	if err == nil {
		err = json.Unmarshal(row.Head, &h)
	}
	if err != nil || h.Latest != snapshotID(1) || h.SavedAtMs == 0 {
		t.Fatalf("ledger head = %+v, %v", h, err)
	}
	ops, err := ledger.Operations(t.Context(), env.svc.ops.all()[0].created.Add(-1))
	if err != nil || len(ops) != 1 || ops[0].FinishedAt.IsZero() || ops[0].Phase != int32(controlv1.Phase_PHASE_SAVING) {
		t.Fatalf("ledger operations = %+v, %v", ops, err)
	}
	var recs []eventRecord
	kept := collapseTransfers(events)
	if err := json.Unmarshal(ops[0].Events, &recs); err != nil || len(recs) != len(kept) || len(kept) >= len(events) || recs[len(recs)-1].Phase != int32(controlv1.Phase_PHASE_DONE) {
		t.Fatalf("ledger events = %+v, %v, want the %d of %d watched events left after collapsing the transfers", recs, err, len(kept), len(events))
	}

	if _, err := ledger.PutHead(t.Context(), alice, aliceBox, []byte(`{"latest":"stale"}`)); err != nil {
		t.Fatalf("PutHead: %v", err)
	}
	ops[0].Events = []byte(`[]`)
	if err := ledger.PutOperation(t.Context(), ops[0]); err != nil {
		t.Fatalf("PutOperation: %v", err)
	}
	d, err := env.svc.Verify(t.Context())
	if err != nil || !slices.Equal(d.Heads, []string{alice + "/" + aliceBox}) || !slices.Equal(d.Operations, []string{ops[0].ID}) {
		t.Fatalf("Verify after tampering = %+v, %v", d, err)
	}
	if d, err := env.svc.Verify(t.Context()); err != nil || len(d.Heads)+len(d.Operations) != 0 {
		t.Errorf("Verify after the repair = %+v, %v, want no drift", d, err)
	}
	op := env.svc.ops.get(ops[0].ID)
	op.mu.Lock()
	op.finishedAt = op.finishedAt.Add(-2 * operationRetention)
	op.mu.Unlock()
	if err := ledger.PurgeOperations(t.Context(), time.Now()); err != nil {
		t.Fatalf("PurgeOperations: %v", err)
	}
	if d, err := env.svc.Verify(t.Context()); err != nil || len(d.Operations) != 0 {
		t.Errorf("Verify with an operation past its retention = %+v, %v, want it skipped", d, err)
	}
}
