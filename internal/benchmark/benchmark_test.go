package benchmark

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Test 6: Determinism: running the whole fake benchmark twice yields identical scores and
// identical rendered report (reflect.DeepEqual / string equality).
func TestDeterminism(t *testing.T) {
	cases := DefaultCases()

	runner1 := NewRunner(RunnerOptions{Repeats: 3})
	scores1, err1 := runner1.Run(context.Background(), cases)
	if err1 != nil {
		t.Fatalf("first benchmark run failed: %v", err1)
	}
	report1 := RenderComparison(cases, scores1)

	runner2 := NewRunner(RunnerOptions{Repeats: 3})
	scores2, err2 := runner2.Run(context.Background(), cases)
	if err2 != nil {
		t.Fatalf("second benchmark run failed: %v", err2)
	}
	report2 := RenderComparison(cases, scores2)

	if !reflect.DeepEqual(scores1, scores2) {
		t.Errorf("scores differ between runs:\nRun 1: %+v\nRun 2: %+v", scores1, scores2)
	}

	if report1 != report2 {
		t.Errorf("rendered reports differ between runs.\nReport 1:\n%s\nReport 2:\n%s", report1, report2)
	}
}

// Test 7: Isolation gate: the product path does not import internal/benchmark.
func TestIsolationGate(t *testing.T) {
	productDirs := []string{
		"../../internal/api",
		"../../internal/worker",
		"../../internal/review",
		"../../internal/storage",
		"../../internal/domain",
		"../../internal/evidence",
		"../../internal/provider",
		"../../cmd/speccouncil",
	}

	targetImport := "internal/benchmark"

	for _, dir := range productDirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}

			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}

			if strings.Contains(string(content), targetImport) {
				t.Errorf("isolation violation: product file %s imports %q", path, targetImport)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("failed to scan %s: %v", dir, err)
		}
	}
}
