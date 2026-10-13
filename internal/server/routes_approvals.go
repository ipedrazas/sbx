package server

// Management API for the approval queue (/manage/approvals). On approve the
// daemon executes the action inline, caches the result under the agent's
// idempotency key so its original call replays cleanly, and moves the row to
// `executed` (or `failed` when the connector errors).

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/factory"
	"github.com/ipedrazas/sbx/internal/models"
)

const approvalIdempotencyTTLSeconds int64 = 86_400

func (s *Server) registerApprovals(mux *http.ServeMux) {
	const p = "/manage/approvals"
	handle(mux, s.wrap(s.listApprovals), "GET "+p, "GET "+p+"/")
	handle(mux, s.wrap(s.getApproval), "GET "+p+"/{id}")
	handle(mux, s.wrap(s.approveApproval), "POST "+p+"/{id}/approve")
	handle(mux, s.wrap(s.denyApproval), "POST "+p+"/{id}/deny")
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) error {
	// v1 only exposes the pending queue.
	if st := queryString(r, "state"); st != nil && *st != "pending" {
		return errBadRequest("unsupported state filter '%s' (v1 supports 'pending')", *st)
	}
	agentID, err := queryInt64(r, "agent_id")
	if err != nil {
		return err
	}
	rows, err := actions.ListPendingApprovals(r.Context(), s.db, agentID)
	if err != nil {
		return errInternal("%v", err)
	}
	if rows == nil {
		rows = []models.ActionApproval{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": rows})
	return nil
}

func (s *Server) loadApproval(ctx context.Context, id int64) (*models.ActionApproval, error) {
	a, err := actions.GetApproval(ctx, s.db, id)
	if err != nil {
		return nil, errInternal("%v", err)
	}
	if a == nil {
		return nil, errNotFound("approval not found")
	}
	return a, nil
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	a, err := s.loadApproval(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a)
	return nil
}

// loadPending loads the approval and 409s unless it is still pending.
func (s *Server) loadPending(ctx context.Context, id int64) (*models.ActionApproval, error) {
	a, err := s.loadApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.State != models.ApprovalPending {
		return nil, errConflict("approval is already %s", a.State)
	}
	return a, nil
}

func decodeResolvedBy(r *http.Request) (string, error) {
	var req struct {
		ResolvedBy *string `json:"resolved_by"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return "", err
	}
	if req.ResolvedBy == nil {
		return "", missingJSONField("resolved_by")
	}
	return *req.ResolvedBy, nil
}

func (s *Server) denyApproval(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	by, err := decodeResolvedBy(r)
	if err != nil {
		return err
	}
	// Load first so a missing row 404s instead of looking like a race.
	a, err := s.loadPending(r.Context(), id)
	if err != nil {
		return err
	}
	ok, err := actions.ResolveApproval(r.Context(), s.db, a.ID, models.ApprovalDenied, nil, &by)
	if err != nil {
		return errInternal("%v", err)
	}
	if !ok {
		return errConflict("approval is no longer pending (race with another approver or the expiry sweep)")
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": models.ApprovalDenied})
	return nil
}

func (s *Server) approveApproval(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	by, err := decodeResolvedBy(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	a, err := s.loadPending(ctx, id)
	if err != nil {
		return err
	}

	// A missing action/datasource terminates the row in Failed so the queue
	// can't hold rows that never resolve.
	action, err := actions.GetAction(ctx, s.db, a.ActionID)
	if err != nil {
		return errInternal("%v", err)
	}
	if action == nil {
		return s.failApproval(ctx, a, by, "action no longer exists")
	}
	ds, err := crud.GetDatasource(ctx, s.db, action.DatasourceID)
	if err != nil {
		return errInternal("%v", err)
	}
	if ds == nil {
		return s.failApproval(ctx, a, by, "action's datasource no longer exists")
	}

	conn, err := factory.CreateConnector(ctx, s.db, ds)
	if err != nil {
		return errBadGateway("%v", err)
	}
	defer factory.Close(conn)

	// Real vs simulated is fixed at queue time.
	var result any
	if a.Simulated {
		result, err = conn.SimulateAction(ctx, action.ActionType, paramsMap(a.Params))
	} else {
		result, err = conn.InvokeAction(ctx, action.ActionType, paramsMap(a.Params))
	}

	if err != nil {
		envelope := map[string]any{"error": err.Error()}
		ok, rerr := actions.ResolveApproval(ctx, s.db, a.ID, models.ApprovalFailed, envelope, &by)
		if rerr != nil {
			return errInternal("%v", rerr)
		}
		if !ok {
			return errConflict("approval state changed mid-approve")
		}
		return errBadGateway("%v", err)
	}

	ok, err := actions.ResolveApproval(ctx, s.db, a.ID, models.ApprovalExecuted, result, &by)
	if err != nil {
		return errInternal("%v", err)
	}
	if !ok {
		return errConflict("approval state changed mid-approve")
	}

	// Cache the result against the agent's idempotency key so its retry replays
	// it. Failure is tolerated — execution already happened.
	if a.IdempotencyKey != nil && a.TokenID != nil {
		ttl := approvalIdempotencyTTLSeconds
		if action.ApprovalTTLSeconds != nil {
			ttl = *action.ApprovalTTLSeconds
		}
		if _, err := actions.InsertIdempotency(ctx, s.db, *a.TokenID, a.ActionID, *a.IdempotencyKey,
			a.Simulated, a.ParamsHash, 200, result, ttl); err != nil {
			slog.Warn("failed to cache executed approval result", "approval_id", a.ID, "err", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"state": models.ApprovalExecuted, "result": result})
	return nil
}

// failApproval resolves the row as Failed with {"error": reason} and returns a 400.
func (s *Server) failApproval(ctx context.Context, a *models.ActionApproval, resolvedBy, reason string) error {
	envelope := map[string]any{"error": reason}
	if _, err := actions.ResolveApproval(ctx, s.db, a.ID, models.ApprovalFailed, envelope, &resolvedBy); err != nil {
		return errInternal("%v", err)
	}
	return errBadRequest("%s", reason)
}
