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

func TestBenchmarkCLIRefusesOverCap(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-provider", "cline", "-max-cost", "0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected cline run with -max-cost 0 to fail; got exit 0")
	}
	output := string(out)
	if !strings.Contains(output, "Estimated cost:") {
		t.Errorf("output missing cost estimate: %s", output)
	}
	if !strings.Contains(output, "exceeds cap") {
		t.Errorf("output missing exceeds cap message: %s", output)
	}
}

func TestBenchmarkCLIClampsHardCeiling(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-provider", "cline", "-max-cost", "5.0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected cline run without -i-accept-cost to fail; got exit 0")
	}
	output := string(out)
	if !strings.Contains(output, "(cap: $3.00)") {
		t.Errorf("expected output to show clamped cap: $3.00, got: %s", output)
	}
}

func TestBenchmarkCLIRefusesMissingOrPlaceholderKey(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-provider", "cline", "-i-accept-cost")
	cmd.Env = append(cmd.Environ(), "SPECCOUNCIL_CLINE_KEY_FILE=nonexistent_key_file")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected cline run with missing key to fail; got exit 0")
	}
	output := string(out)
	if !strings.Contains(output, "read cline key file") && !strings.Contains(output, "empty or placeholder") {
		t.Errorf("expected key error message, got: %s", output)
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
