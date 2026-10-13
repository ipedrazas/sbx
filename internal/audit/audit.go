// Package audit records agent-facing requests in the audit_log table.
//
// The request path never blocks on DB writes: middleware pushes AuditEvents
// onto a buffered channel and a background goroutine (SpawnWriter) batches
// inserts every 500ms or 100 rows. On a crash we lose at most the current
// batch — acceptable since audit is observational, not authoritative.
package audit

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// AuditEvent is one entry in audit_log. Fields mirror the columns.
type AuditEvent struct {
	AgentID   *int64
	DatasetID *int64
	Alias     *string
	Endpoint  string
	Status    uint16
	BytesOut  uint64
	LatencyMs uint32
	// Action-side fields; nil on dataset-read rows.
	ActionID       *int64
	ApprovalID     *int64
	IdempotencyKey *string
	ParamsHash     *string
	Simulated      bool
	ApprovalState  *string
}

const (
	batchMax      = 100
	flushInterval = 500 * time.Millisecond
	// channelCap bounds memory; Rust used an unbounded channel. Events beyond
	// this backlog are dropped rather than blocking the request path.
	channelCap    = 10000
	pruneInterval = 24 * time.Hour
)

// Sender is the cheap handle request handlers hold. The zero value drops
// every event.
type Sender struct {
	ch   chan<- AuditEvent
	done <-chan struct{}
}

// Send enqueues ev without blocking. It reports false when the event was
// dropped (writer backlog full, or zero Sender).
func (s Sender) Send(ev AuditEvent) bool {
	if s.ch == nil {
		return false
	}
	select {
	case s.ch <- ev:
		return true
	default:
		return false
	}
}

// Wait blocks until the writer goroutine has flushed and exited, which
// happens after the context passed to SpawnWriter is cancelled.
func (s Sender) Wait() {
	if s.done != nil {
		<-s.done
	}
}

// SpawnWriter starts the background batch writer. It stops (after flushing
// whatever is buffered) when ctx is cancelled.
func SpawnWriter(ctx context.Context, db *sql.DB) Sender {
	ch := make(chan AuditEvent, channelCap)
	done := make(chan struct{})
	go runWriter(ctx, db, ch, done)
	return Sender{ch: ch, done: done}
}

func runWriter(ctx context.Context, db *sql.DB, ch <-chan AuditEvent, done chan<- struct{}) {
	defer close(done)
	// Writes must survive shutdown cancellation so the final flush lands.
	wctx := context.WithoutCancel(ctx)
	buf := make([]AuditEvent, 0, batchMax)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case ev := <-ch:
			buf = append(buf, ev)
			if len(buf) >= batchMax {
				buf = flush(wctx, db, buf)
			}
		case <-ticker.C:
			if len(buf) > 0 {
				buf = flush(wctx, db, buf)
			}
		case <-ctx.Done():
			// Drain what is already queued, flush, and exit.
			for {
				select {
				case ev := <-ch:
					buf = append(buf, ev)
					if len(buf) >= batchMax {
						buf = flush(wctx, db, buf)
					}
				default:
					flush(wctx, db, buf)
					return
				}
			}
		}
	}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// flush writes buf as a single multi-row INSERT and returns buf emptied.
func flush(ctx context.Context, db *sql.DB, buf []AuditEvent) []AuditEvent {
	if len(buf) == 0 {
		return buf
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO audit_log (agent_id, dataset_id, alias, endpoint, status, bytes_out, latency_ms, " +
		"action_id, approval_id, idempotency_key, params_hash, simulated, approval_state) VALUES ")
	args := make([]any, 0, len(buf)*13)
	for i, ev := range buf {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, ev.AgentID, ev.DatasetID, ev.Alias, ev.Endpoint, int64(ev.Status),
			int64(ev.BytesOut), int64(ev.LatencyMs), ev.ActionID, ev.ApprovalID, ev.IdempotencyKey,
			ev.ParamsHash, boolInt(ev.Simulated), ev.ApprovalState)
	}
	if _, err := db.ExecContext(ctx, sb.String(), args...); err != nil {
		slog.Warn("audit flush failed", "dropped", len(buf), "error", err)
	}
	return buf[:0]
}

// AuditRow is one persisted audit row, as returned by the management API.
type AuditRow struct {
	ID        int64   `json:"id"`
	Ts        string  `json:"ts"`
	AgentID   *int64  `json:"agent_id"`
	DatasetID *int64  `json:"dataset_id"`
	Alias     *string `json:"alias"`
	Endpoint  string  `json:"endpoint"`
	Status    int64   `json:"status"`
	BytesOut  int64   `json:"bytes_out"`
	LatencyMs int64   `json:"latency_ms"`
}

func whereClause(agentID, datasetID *int64) (string, []any) {
	var conds []string
	var args []any
	if agentID != nil {
		conds = append(conds, "agent_id = ?")
		args = append(args, *agentID)
	}
	if datasetID != nil {
		conds = append(conds, "dataset_id = ?")
		args = append(args, *datasetID)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListEvents returns recent audit rows, newest first, optionally filtered by
// agent and/or dataset. The caller caps limit.
func ListEvents(ctx context.Context, db *sql.DB, agentID, datasetID *int64, limit, offset int64) ([]AuditRow, error) {
	where, args := whereClause(agentID, datasetID)
	q := "SELECT id, ts, agent_id, dataset_id, alias, endpoint, status, bytes_out, latency_ms FROM audit_log" +
		where + " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var (
			r        AuditRow
			aid, did sql.NullInt64
			alias    sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Ts, &aid, &did, &alias, &r.Endpoint, &r.Status, &r.BytesOut, &r.LatencyMs); err != nil {
			return nil, err
		}
		if aid.Valid {
			r.AgentID = &aid.Int64
		}
		if did.Valid {
			r.DatasetID = &did.Int64
		}
		if alias.Valid {
			r.Alias = &alias.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountEvents counts audit rows matching the same filters as ListEvents.
func CountEvents(ctx context.Context, db *sql.DB, agentID, datasetID *int64) (int64, error) {
	where, args := whereClause(agentID, datasetID)
	var n int64
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_log"+where, args...).Scan(&n)
	return n, err
}

// PruneOlderThan deletes rows older than retainDays and returns how many went.
func PruneOlderThan(ctx context.Context, db *sql.DB, retainDays uint32) (int64, error) {
	modifier := "-" + strconv.FormatUint(uint64(retainDays), 10) + " days"
	res, err := db.ExecContext(ctx, "DELETE FROM audit_log WHERE ts < datetime('now', ?)", modifier)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SpawnPruner prunes audit_log once now and then every 24h until ctx is
// cancelled. A nil retainDays disables pruning (the table grows forever).
func SpawnPruner(ctx context.Context, db *sql.DB, retainDays *uint32) {
	if retainDays == nil {
		slog.Info("audit retention disabled; audit_log will grow unbounded")
		return
	}
	go runPruner(ctx, db, *retainDays, pruneInterval)
}

func runPruner(ctx context.Context, db *sql.DB, days uint32, interval time.Duration) {
	for {
		n, err := PruneOlderThan(ctx, db, days)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			slog.Warn("audit prune failed", "error", err)
		case n == 0:
			slog.Debug("audit prune: no rows older than retention", "days", days)
		default:
			slog.Info("audit prune: deleted rows older than retention", "deleted", n, "days", days)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
