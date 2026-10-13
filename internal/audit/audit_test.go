package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/ipedrazas/sbx/internal/db"
)

func setupDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	pool, err := db.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	return ctx, pool
}

func exec(t *testing.T, pool *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestPruneDeletesOldRowsOnly(t *testing.T) {
	ctx, pool := setupDB(t)
	exec(t, pool, "INSERT INTO audit_log (ts, agent_id, dataset_id, alias, endpoint, status, bytes_out, latency_ms) VALUES ('2020-01-01 00:00:00', 1, 1, 'a', 'old', 200, 0, 0)")
	exec(t, pool, "INSERT INTO audit_log (agent_id, dataset_id, alias, endpoint, status, bytes_out, latency_ms) VALUES (1, 1, 'a', 'new', 200, 0, 0)")

	deleted, err := PruneOlderThan(ctx, pool, 30)
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if n, _ := CountEvents(ctx, pool, nil, nil); n != 1 {
		t.Fatal(n)
	}
	events, err := ListEvents(ctx, pool, nil, nil, 10, 0)
	if err != nil || len(events) != 1 || events[0].Endpoint != "new" {
		t.Fatalf("%+v %v", events, err)
	}
}

func TestPruneOnEmptyTableIsNoop(t *testing.T) {
	ctx, pool := setupDB(t)
	if n, err := PruneOlderThan(ctx, pool, 30); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestListFiltersByAgentAndDataset(t *testing.T) {
	ctx, pool := setupDB(t)
	exec(t, pool, "INSERT INTO audit_log (agent_id, dataset_id, alias, endpoint, status, bytes_out, latency_ms) VALUES "+
		"(1, 10, 'a', 'list', 200, 0, 0), (1, 20, 'b', 'list', 200, 0, 0), (2, 10, 'a', 'list', 200, 0, 0)")

	cases := []struct {
		agent, dataset *int64
		want           int
	}{
		{ptr(int64(1)), nil, 2},
		{nil, ptr(int64(10)), 2},
		{ptr(int64(1)), ptr(int64(10)), 1},
		{nil, nil, 3},
	}
	for _, c := range cases {
		rows, err := ListEvents(ctx, pool, c.agent, c.dataset, 10, 0)
		if err != nil || len(rows) != c.want {
			t.Fatalf("list %v/%v: %d %v", c.agent, c.dataset, len(rows), err)
		}
		n, err := CountEvents(ctx, pool, c.agent, c.dataset)
		if err != nil || n != int64(c.want) {
			t.Fatalf("count %v/%v: %d %v", c.agent, c.dataset, n, err)
		}
	}
	// Newest first, limit/offset honored.
	rows, _ := ListEvents(ctx, pool, nil, nil, 1, 1)
	if len(rows) != 1 || rows[0].ID != 2 {
		t.Fatalf("%+v", rows)
	}
}

func TestAuditRowJSONShape(t *testing.T) {
	b, _ := json.Marshal(AuditRow{ID: 1, Ts: "t", Endpoint: "e"})
	want := `{"id":1,"ts":"t","agent_id":null,"dataset_id":null,"alias":null,"endpoint":"e","status":0,"bytes_out":0,"latency_ms":0}`
	if string(b) != want {
		t.Fatal(string(b))
	}
}

func TestWriterFlushesOnShutdown(t *testing.T) {
	_, pool := setupDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	tx := SpawnWriter(ctx, pool)
	for i := 0; i < 5; i++ {
		if !tx.Send(AuditEvent{AgentID: ptr(int64(7)), Alias: ptr("x"), Endpoint: "list", Status: 200, BytesOut: 12, LatencyMs: 3}) {
			t.Fatal("send dropped")
		}
	}
	tx.Send(AuditEvent{
		Endpoint: "act", Status: 202, ActionID: ptr(int64(3)), ApprovalID: ptr(int64(4)),
		IdempotencyKey: ptr("k"), ParamsHash: ptr("h"), Simulated: true, ApprovalState: ptr("pending"),
	})
	cancel()
	tx.Wait()

	n, err := CountEvents(context.Background(), pool, ptr(int64(7)), nil)
	if err != nil || n != 5 {
		t.Fatalf("%d %v", n, err)
	}
	var (
		sim      int64
		state    string
		actionID int64
	)
	if err := pool.QueryRow("SELECT simulated, approval_state, action_id FROM audit_log WHERE endpoint = 'act'").Scan(&sim, &state, &actionID); err != nil {
		t.Fatal(err)
	}
	if sim != 1 || state != "pending" || actionID != 3 {
		t.Fatal(sim, state, actionID)
	}
}

func TestWriterFlushesOnTickAndBatch(t *testing.T) {
	_, pool := setupDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx := SpawnWriter(ctx, pool)
	// More than one batch: the first 100 flush immediately, the rest on tick.
	for i := 0; i < batchMax+10; i++ {
		tx.Send(AuditEvent{Endpoint: "e", Status: 200})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, _ := CountEvents(context.Background(), pool, nil, nil)
		if n == int64(batchMax+10) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d rows flushed", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestZeroSenderDrops(t *testing.T) {
	var s Sender
	if s.Send(AuditEvent{}) {
		t.Fatal("zero sender must drop")
	}
	s.Wait() // must not block
}

func TestSendDropsWhenFull(t *testing.T) {
	ch := make(chan AuditEvent, 1)
	s := Sender{ch: ch}
	if !s.Send(AuditEvent{}) || s.Send(AuditEvent{}) {
		t.Fatal("second send must drop when the buffer is full")
	}
}

func TestFlushErrorIsLogged(t *testing.T) {
	_, pool := setupDB(t)
	pool.Close()
	buf := flush(context.Background(), pool, []AuditEvent{{Endpoint: "x"}})
	if len(buf) != 0 {
		t.Fatal("buffer must be cleared even on failure")
	}
}

func TestPruner(t *testing.T) {
	_, pool := setupDB(t)
	exec(t, pool, "INSERT INTO audit_log (ts, endpoint, status) VALUES ('2020-01-01 00:00:00', 'old', 200)")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	SpawnPruner(ctx, pool, nil) // disabled: no-op

	done := make(chan struct{})
	go func() { runPruner(ctx, pool, 30, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, _ := CountEvents(context.Background(), pool, nil, nil)
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pruner did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	// Public entrypoint with retention set starts and stops cleanly.
	ctx2, cancel2 := context.WithCancel(context.Background())
	SpawnPruner(ctx2, pool, ptr(uint32(30)))
	cancel2()
}
