package ingest_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/ingest"
)

const snapshotSource = "# Design\n\nREQ-12 the system must ...\n\n- Keep records\n| key | value |\n```go\n  enforce()\n```"

func TestBuildSnapshotHappyPath(t *testing.T) {
	got, err := ingest.BuildSnapshot("snap-1", snapshotSource)
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot.ID != "snap-1" || got.Snapshot.Hash == "" || got.SplitterVersion != "1" {
		t.Fatalf("unexpected snapshot metadata: %+v", got)
	}
	blocks := ingest.ParseBlocks(snapshotSource)
	if len(blocks) != 5 || len(got.Snapshot.Units) != len(blocks) {
		t.Fatalf("blocks = %d, units = %d; want 5 of each", len(blocks), len(got.Snapshot.Units))
	}
	for i, block := range ingest.AssignIDs(blocks) {
		unit := got.Snapshot.Units[i]
		if unit.ID != block.ID || unit.Text != block.Block.Text || !got.Snapshot.HasRef(block.ID) {
			t.Errorf("block %d lost ID, text, order, or reference: %+v", i, unit)
		}
		if !evidence.IsValidUnitKind(unit.Kind) {
			t.Errorf("unit %d has invalid kind %q", i, unit.Kind)
		}
	}
	if got.Snapshot.Units[1].ID != "REQ-12" || !got.Snapshot.HasRef("REQ-12") {
		t.Fatal("author ID REQ-12 is not preserved and addressable")
	}
	if got.Snapshot.Units[3].Kind != "data_rule" || got.Snapshot.Units[4].Kind != "constraint" {
		t.Fatalf("table/code kinds = %q/%q; want data_rule/constraint", got.Snapshot.Units[3].Kind, got.Snapshot.Units[4].Kind)
	}
}

func TestBuildSnapshotNoEvidenceUnits(t *testing.T) {
	for _, source := range []string{"", "   ", "\n\n", "-\n*\n1.\n```\n```"} {
		for _, id := range []string{"snap-1", ""} {
			t.Run(id+"/"+source, func(t *testing.T) {
				got, err := ingest.BuildSnapshot(id, source)
				if !errors.Is(err, ingest.ErrNoEvidenceUnits) || err != ingest.ErrNoEvidenceUnits {
					t.Fatalf("error = %v; want exact ErrNoEvidenceUnits before Freeze", err)
				}
				if !reflect.DeepEqual(got, ingest.IngestResult{}) {
					t.Fatalf("error result is not zero: %+v", got)
				}
			})
		}
	}
}

func TestBuildSnapshotFreezeErrors(t *testing.T) {
	for _, tc := range []struct {
		name, id, source, message string
	}{
		{"empty id", "", "Valid evidence", "evidence: snapshot id must not be empty"},
		{"blank id", "  ", "Valid evidence", "evidence: snapshot id must not be empty"},
		{"blank code text", "snap-1", "```\n   \n```", "evidence: unit \"u0\" has empty text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ingest.BuildSnapshot(tc.id, tc.source)
			if err == nil || errors.Is(err, ingest.ErrNoEvidenceUnits) {
				t.Fatalf("error = %v; want Freeze error, not sentinel", err)
			}
			if err.Error() != "ingest: build snapshot: "+tc.message {
				t.Fatalf("unexpected error: %v", err)
			}
			cause := errors.Unwrap(err)
			if cause == nil || cause.Error() != tc.message || !errors.Is(err, cause) {
				t.Fatalf("Freeze error was not preserved with %%w: %v", err)
			}
			if !reflect.DeepEqual(got, ingest.IngestResult{}) {
				t.Fatalf("error result is not zero: %+v", got)
			}
		})
	}
}

func TestBuildSnapshotDeterminism(t *testing.T) {
	first, err := ingest.BuildSnapshot("snap-1", snapshotSource)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ingest.BuildSnapshot("snap-1", snapshotSource)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("results differ: %+v versus %+v", first, second)
	}
}

func TestBuildSnapshotMatchesDirectFreeze(t *testing.T) {
	units := []evidence.Unit{
		{ID: "u0", Kind: "brief", Text: "Design"},
		{ID: "REQ-12", Kind: "requirement", Text: "REQ-12 the system must ..."},
		{ID: "u2", Kind: "requirement", Text: "Keep records"},
		{ID: "u3", Kind: "data_rule", Text: "| key | value |"},
		{ID: "u4", Kind: "constraint", Text: "  enforce()"},
	}
	want, err := evidence.Freeze("snap-1", units)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ingest.BuildSnapshot("snap-1", snapshotSource)
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot.Hash != want.Hash || !reflect.DeepEqual(got.Snapshot, want) {
		t.Fatalf("snapshot differs from direct Freeze: got %+v, want %+v", got.Snapshot, want)
	}
}
