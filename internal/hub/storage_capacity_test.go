package hub

import "testing"

func TestStorageIdentityCapacityWarnsWithoutDeletingReplayFences(t *testing.T) {
	h, _, _, _ := receiptLookupFixture(t)
	// Use real persisted rows, including an expired deadline. Content cleanup
	// must not turn an already issued identity into permission to execute again.
	insert := func(start, end int) {
		t.Helper()
		_, err := h.durable.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(?) UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO operations(id,owner,intent,hash,deadline,ticket) SELECT printf('capacity-%d',x),'capacity-owner',printf('intent-%d',x),'fixture',1,'fixture' FROM n`, start, end)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(1, maxOwnerDurableOperations*80/100)
	assertCapacity := func(want int64, full bool) {
		t.Helper()
		status := h.DurableStorageStatus()
		if !status.CapacityKnown || !status.Warning || status.CapacityMessage == "" {
			t.Fatalf("missing capacity warning: %+v", status)
		}
		for _, r := range status.RecordCapacity {
			if r.Kind == "owner_operations" {
				if r.Used != want || r.Limit != maxOwnerDurableOperations || !r.Warning || r.Full != full {
					t.Fatalf("wrong capacity: %+v", r)
				}
				return
			}
		}
		t.Fatal("owner capacity missing")
	}
	assertCapacity(maxOwnerDurableOperations*80/100, false)
	insert(maxOwnerDurableOperations*80/100+1, maxOwnerDurableOperations)
	assertCapacity(maxOwnerDurableOperations, true)
	if err := h.RunStorageCleanup(); err != nil {
		t.Fatal(err)
	}
	assertCapacity(maxOwnerDurableOperations, true)
	// An unreadable database must not look empty/healthy to administrators.
	if err := h.durable.db.Close(); err != nil {
		t.Fatal(err)
	}
	status := h.DurableStorageStatus()
	if status.CapacityKnown || !status.Warning || len(status.RecordCapacity) != 0 || status.CapacityMessage == "" {
		t.Fatalf("failed read hidden: %+v", status)
	}
}
