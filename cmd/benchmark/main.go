package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/benchmark"
	"github.com/wyze69-sys/SpecCouncil/internal/provider/cline"
)

const (
	DefaultMaxCost   = 0.50
	HardCostCeiling  = 3.00
	DefaultMaxTokens = 1024
)

func main() {
	providerFlag := flag.String("provider", "fake", "provider to use: fake, cline")
	repeatsFlag := flag.Int("repeats", 3, "number of repeats per arm per case")
	acceptCostFlag := flag.Bool("i-accept-cost", false, "acknowledge and accept live provider cost")
	maxCostFlag := flag.Float64("max-cost", DefaultMaxCost, "maximum allowed cost in USD (clamped to <=3.00 hard ceiling)")
	outFlag := flag.String("out", "docs/cline/m2/results", "directory to persist benchmark results")
	flag.Parse()

	if *repeatsFlag < 1 {
		fmt.Fprintln(os.Stderr, "repeats must be at least 1")
		os.Exit(1)
	}

	cases := benchmark.DefaultCases()

	// Guardrail for live runs
	if *providerFlag == "cline" {
		effectiveCap := *maxCostFlag
		if effectiveCap > HardCostCeiling {
			effectiveCap = HardCostCeiling
		}

		estCost, totalCalls := benchmark.EstimateLiveCost(cases, *repeatsFlag, DefaultMaxTokens)
		fmt.Printf("Estimated cost: $%.4f for %d calls across %d cases, %d repeats, 4 arms (cap: $%.2f)\n",
			estCost, totalCalls, len(cases), *repeatsFlag, effectiveCap)

		if estCost > effectiveCap {
			fmt.Fprintf(os.Stderr, "error: estimated cost $%.4f exceeds cap $%.2f (hard ceiling $%.2f); refusing to run\n",
				estCost, effectiveCap, HardCostCeiling)
			os.Exit(1)
		}

		if !*acceptCostFlag {
			fmt.Fprintln(os.Stderr, "error: refusing to run live benchmark without explicit -i-accept-cost flag")
			os.Exit(1)
		}

		keyFile := os.Getenv("SPECCOUNCIL_CLINE_KEY_FILE")
		if keyFile == "" {
			keyFile = ".secrets/cline_api_key"
		}
		data, err := os.ReadFile(keyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read cline key file %s: %v\n", keyFile, err)
			os.Exit(1)
		}
		apiKey := strings.TrimSpace(string(data))
		if apiKey == "" || strings.HasPrefix(apiKey, "PLACEHOLDER") {
			fmt.Fprintf(os.Stderr, "error: cline api key in %s is empty or placeholder\n", keyFile)
			os.Exit(1)
		}

		clineProv, err := cline.New(cline.Config{
			BaseURL:   "https://api.cline.bot/api/v1",
			APIKey:    apiKey,
			Model:     "deepseek/deepseek-v4.1-flash",
			Timeout:   60 * time.Second,
			MaxTokens: DefaultMaxTokens,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: init cline provider: %v\n", err)
			os.Exit(1)
		}

		ctx := context.Background()
		var allRuns []benchmark.RunResult
		var scores []benchmark.ArmScore
		arms := []benchmark.Arm{
			benchmark.ArmFreeform,
			benchmark.ArmStructured,
			benchmark.ArmRoles,
			benchmark.ArmGeneric,
		}

		for _, c := range cases {
			for _, arm := range arms {
				runs := make([]benchmark.RunResult, 0, *repeatsFlag)
				for repeat := 1; repeat <= *repeatsFlag; repeat++ {
					res, err := benchmark.ExecuteArm(ctx, clineProv, arm, c, repeat)
					if err != nil {
						fmt.Fprintf(os.Stderr, "benchmark run failed: execute arm %s on case %s repeat %d failed: %v\n", arm, c.ID, repeat, err)
						os.Exit(1)
					}
					runs = append(runs, res)
					allRuns = append(allRuns, res)
				}
				score := benchmark.AggregateArm(arm, c, runs)
				scores = append(scores, score)
			}
		}

		report := benchmark.RenderComparison(cases, scores)
		fmt.Println(report)

		if err := os.MkdirAll(*outFlag, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "error: create output dir %s: %v\n", *outFlag, err)
			os.Exit(1)
		}

		utcStamp := time.Now().UTC().Format("20060102T150405Z")

		rawJSON, err := json.MarshalIndent(allRuns, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: marshal run results: %v\n", err)
			os.Exit(1)
		}
		runPath := filepath.Join(*outFlag, fmt.Sprintf("run-%s.json", utcStamp))
		if err := os.WriteFile(runPath, rawJSON, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error: write run results to %s: %v\n", runPath, err)
			os.Exit(1)
		}

		compPath := filepath.Join(*outFlag, fmt.Sprintf("comparison-%s.md", utcStamp))
		if err := os.WriteFile(compPath, []byte(report), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error: write comparison report to %s: %v\n", compPath, err)
			os.Exit(1)
		}

		return
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
