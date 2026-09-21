package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/wyze69-sys/SpecCouncil/internal/benchmark"
)

func main() {
	providerFlag := flag.String("provider", "fake", "provider to use: fake, cline")
	repeatsFlag := flag.Int("repeats", 3, "number of repeats per arm per case")
	acceptCostFlag := flag.Bool("i-accept-cost", false, "acknowledge and accept live provider cost")
	flag.Parse()

	if *repeatsFlag < 1 {
		fmt.Fprintln(os.Stderr, "repeats must be at least 1")
		os.Exit(1)
	}

	cases := benchmark.DefaultCases()

	// Guardrail for live runs
	if *providerFlag == "cline" {
		// Budget: freeform=1, structured=1, roles=4, generic=4 => 10 calls per repeat
		totalCalls := 10 * (*repeatsFlag) * len(cases)
		estCost := float64(totalCalls) * 0.03 // coarse estimate ~$0.03/call
		fmt.Printf("Estimated cost: $%.2f for %d calls across %d cases, %d repeats, 4 arms\n",
			estCost, totalCalls, len(cases), *repeatsFlag)

		if !*acceptCostFlag {
			fmt.Fprintln(os.Stderr, "error: refusing to run live benchmark without explicit -i-accept-cost flag")
			os.Exit(1)
		}

		// M2-4b: wire live Cline provider here
		fmt.Fprintln(os.Stderr, "error: cline provider reserved for M2-4b; live paid runs not enabled in M2-4a")
		os.Exit(1)
	}

	if *providerFlag != "fake" {
		fmt.Fprintf(os.Stderr, "error: unknown provider %q (supported: fake, cline)\n", *providerFlag)
		os.Exit(1)
	}

	// Default fake provider runner
	runner := benchmark.NewRunner(benchmark.RunnerOptions{
		Repeats: *repeatsFlag,
	})

	scores, err := runner.Run(context.Background(), cases)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark run failed: %v\n", err)
		os.Exit(1)
	}

	report := benchmark.RenderComparison(cases, scores)
	fmt.Println(report)
}
