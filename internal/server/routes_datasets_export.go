package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/exportdata"
)

// datasetsSlug turns a dataset name into a filename stem: lowercase ASCII
// alphanumerics, runs of anything else collapse to `-`, edges trimmed, empty
// falls back to `dataset`.
func datasetsSlug(name string) string {
	var b strings.Builder
	prevDash := false
	for _, c := range name {
		switch {
		case c < 128 && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9'):
			b.WriteRune(c)
			prevDash = false
		case c >= 'A' && c <= 'Z':
			b.WriteRune(c + ('a' - 'A'))
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "dataset"
}

type downloadRequest struct {
	// csv (default), json or jsonl.
	Format *string `json:"format"`
	// When set, the file is written into this directory and its path returned
	// (GUI flow); otherwise the bytes are streamed back as an attachment.
	OutputDir *string `json:"output_dir"`
}

func (s *Server) downloadDataset(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	var req downloadRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	fs := "csv"
	if req.Format != nil {
		fs = *req.Format
	}
	format, err := exportdata.ParseFormat(fs)
	if err != nil {
		return errBadRequest("%s", err.Error())
	}
	ctx := r.Context()
	dataset, err := crud.GetDataset(ctx, s.db, id)
	if err != nil {
		return err
	}
	if dataset == nil {
		return errNotFound("dataset not found")
	}
	filename := datasetsSlug(dataset.Name) + "." + format.Extension()

	if req.OutputDir != nil {
		rows, err := s.liveCollectAllServedRows(ctx, id)
		if err != nil {
			return err
		}
		data, err := exportdata.SerializeRows(rows, format)
		if err != nil {
			return errInternal("%s", err.Error())
		}
		path := filepath.Join(*req.OutputDir, filename)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return errInternal("failed to write %s: %v", filename, err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": path, "rows": len(rows), "format": format.Extension()})
		return nil
	}
	stream, err := s.liveStreamServedRows(ctx, id, format)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", format.ContentType())
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	w.WriteHeader(http.StatusOK)
	if err := stream(w); err != nil {
		// The 200 is committed; abort the connection so the client sees a
		// truncated download rather than a clean EOF.
		slog.Error("dataset download stream failed", "dataset_id", id, "err", err)
		panic(http.ErrAbortHandler)
	}
	return nil
}
