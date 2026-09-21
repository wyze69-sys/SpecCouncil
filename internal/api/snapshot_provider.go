package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/ingest"
)

// NewIngestSnapshotProvider returns a SnapshotProvider that builds a frozen
// evidence snapshot from the submitted content using the deterministic
// ingestion pipeline (ingest.BuildSnapshot).
//
// The snapshot ID is derived deterministically from (projectID, title, content)
// so identical resubmissions reuse the same snapshot row, and distinct projects
// or titles never collide on one ID. The ingestion splitter version is not
// persisted in this slice (submit writes normalization_version = 1); it is
// intentionally dropped here.
//
// It returns ingest.ErrNoEvidenceUnits unchanged when the content yields no
// evidence, so the caller can map it to a client error.
func NewIngestSnapshotProvider() SnapshotProvider {
	return func(ctx context.Context, projectID, title, content string) (evidence.Snapshot, error) {
		id := snapshotID(projectID, title, content)
		res, err := ingest.BuildSnapshot(id, content)
		if err != nil {
			return evidence.Snapshot{}, err
		}
		return res.Snapshot, nil
	}
}

// snapshotID derives a stable, project-scoped snapshot identifier.
func snapshotID(projectID, title, content string) string {
	h := sha256.New()
	// NUL separators keep the fields unambiguous across concatenation.
	h.Write([]byte(projectID))
	h.Write([]byte{0})
	h.Write([]byte(title))
	h.Write([]byte{0})
	h.Write([]byte(content))
	return "snap-" + hex.EncodeToString(h.Sum(nil))
}
