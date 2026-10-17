package boxes

import (
	"bytes"
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

	if err := ledger.PutHead(t.Context(), alice, aliceBox, []byte(`{"latest":"stale"}`)); err != nil {
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
	op := env.svc.ops.local(ops[0].ID)
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

func TestBackfillHeadsMakesTheLedgerMatchTheStore(t *testing.T) {
	ledger := clustertest.Open(t)
	st := newFakeStore()
	a, b := statePath{user: alice, box: "cb-00000000000a"}, statePath{user: alice, box: "cb-00000000000b"}
	for _, p := range []statePath{a, b} {
		if err := putJSON(t.Context(), st, p.head(), head{Latest: snapshotID(2), SavedAtMs: 200}); err != nil {
			t.Fatalf("put head: %v", err)
		}
	}
	if err := ledger.PutHead(t.Context(), alice, a.box, headBytes(head{Latest: snapshotID(1), SavedAtMs: 100})); err != nil {
		t.Fatalf("PutHead stale: %v", err)
	}
	if err := ledger.PutHead(t.Context(), alice, "cb-00000000000z", headBytes(head{Latest: snapshotID(1)})); err != nil {
		t.Fatalf("PutHead ghost: %v", err)
	}
	if written, removed, err := BackfillHeads(t.Context(), st, ledger, alice); err != nil || written != 2 || removed != 1 {
		t.Fatalf("BackfillHeads = %d written, %d removed, %v; want 2 and 1", written, removed, err)
	}
	rows, err := ledger.Heads(t.Context())
	if err != nil || len(rows) != 2 {
		t.Fatalf("ledger heads after the backfill = %+v, %v", rows, err)
	}
	for _, r := range rows {
		if !bytes.Equal(r.Head, headBytes(head{Latest: snapshotID(2), SavedAtMs: 200})) {
			t.Errorf("ledger head of %s = %s, want the store's", r.Box, r.Head)
		}
	}
	if written, removed, err := BackfillHeads(t.Context(), st, ledger, alice); err != nil || written+removed != 0 {
		t.Errorf("second BackfillHeads = %d written, %d removed, %v; want nothing to do", written, removed, err)
	}
}
