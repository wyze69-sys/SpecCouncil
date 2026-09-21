package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBenchmarkCLIRefusesLiveWithoutAcceptCost(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-provider", "cline")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected cline run without -i-accept-cost to fail; got exit 0")
	}
	output := string(out)
	if !strings.Contains(output, "Estimated cost:") {
		t.Errorf("output missing cost estimate: %s", output)
	}
	if !strings.Contains(output, "refusing to run live benchmark without explicit -i-accept-cost flag") {
		t.Errorf("output missing refusal message: %s", output)
	}
}

func TestBenchmarkCLIRefusesLiveEvenWithAcceptCostInM2_4a(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-provider", "cline", "-i-accept-cost")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected cline run in M2-4a to fail; got exit 0")
	}
	output := string(out)
	if !strings.Contains(output, "cline provider reserved for M2-4b") {
		t.Errorf("output missing M2-4b reservation message: %s", output)
	}
}

func TestBenchmarkCLIFakeRunsCleanly(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-provider", "fake", "-repeats", "1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected fake run to exit 0; failed with: %v\nOutput: %s", err, string(out))
	}
	output := string(out)
	if !strings.Contains(output, "# Benchmark Comparison") {
		t.Errorf("output missing report header: %s", output)
	}
	if !strings.Contains(output, "## Telemetry (non-baseline)") {
		t.Errorf("output missing telemetry header: %s", output)
	}
}
