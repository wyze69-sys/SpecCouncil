package ingest_test

import (
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/ingest"
)

// TestSplitterVersionIsExactLiteral pins the recorded version string itself, so
// a value change cannot pass as an implementation detail.
func TestSplitterVersionIsExactLiteral(t *testing.T) {
	if ingest.SplitterVersion != "1" {
		t.Fatalf("SplitterVersion = %q, want %q", ingest.SplitterVersion, "1")
	}
}

// TestSplitterReturnsSplitterVersion pins that the accessor returns exactly the
// constant, and the exact literal, so a constant the accessor ignores cannot
// pass.
func TestSplitterReturnsSplitterVersion(t *testing.T) {
	if got := ingest.Splitter(); got != ingest.SplitterVersion {
		t.Errorf("Splitter() = %q, want SplitterVersion %q", got, ingest.SplitterVersion)
	}
	if got := ingest.Splitter(); got != "1" {
		t.Errorf("Splitter() = %q, want %q", got, "1")
	}
}

// TestSplitterVersionIsNonEmpty pins the non-empty guarantee: a recorded
// version must never be blank, because a blank version cannot identify the
// producing algorithm.
func TestSplitterVersionIsNonEmpty(t *testing.T) {
	if ingest.SplitterVersion == "" {
		t.Fatal("SplitterVersion is empty, want a non-empty version")
	}
	if got := ingest.Splitter(); got == "" {
		t.Fatal("Splitter() returned an empty string, want a non-empty version")
	}
}

// TestSplitterIsDeterministic pins purity: repeated calls return the same
// value, so the version cannot depend on call count, order, or environment.
func TestSplitterIsDeterministic(t *testing.T) {
	first := ingest.Splitter()
	second := ingest.Splitter()
	if first != second {
		t.Fatalf("Splitter() is not deterministic: %q then %q", first, second)
	}
}
