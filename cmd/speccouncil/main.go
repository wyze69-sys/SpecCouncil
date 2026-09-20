// Command speccouncil runs one design review against a frozen snapshot.
//
// Milestone 1 scope: four roles, sequential execution, FakeProvider only.
// There is no HTTP server, no database and no real provider yet.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/provider/fake"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
)

// snapshotFile is the on-disk shape of a design snapshot.
type snapshotFile struct {
	ID    string          `json:"id"`
	Units []evidence.Unit `json:"units"`
}

// scriptedCall is one canned provider response in a script file.
type scriptedCall struct {
	Body           string               `json:"body"`
	TransportError domain.ErrorCategory `json:"transport_error"`
	Message        string               `json:"message"`
}

// scriptFile maps each role to its ordered provider answers.
type scriptFile map[domain.Role][]scriptedCall

func main() {
	snapshotPath := flag.String("snapshot", "", "snapshot JSON file (omit for the built-in demo)")
	scriptPath := flag.String("script", "", "fake-provider script JSON file (omit for the built-in demo)")
	sessionID := flag.String("session", "demo-session", "review session id")
	flag.Parse()

	if err := run(*snapshotPath, *scriptPath, *sessionID); err != nil {
		fmt.Fprintln(os.Stderr, "speccouncil:", err)
		os.Exit(1)
	}
}

func run(snapshotPath, scriptPath, sessionID string) error {
	var snapFile snapshotFile
	var script scriptFile

	if snapshotPath == "" {
		snapFile = demoSnapshot()
	} else {
		if err := readJSON(snapshotPath, &snapFile); err != nil {
			return err
		}
	}

	if scriptPath == "" {
		script = demoScript()
	} else {
		if err := readJSON(scriptPath, &script); err != nil {
			return err
		}
	}

	snap, err := evidence.Freeze(snapFile.ID, snapFile.Units)
	if err != nil {
		return err
	}

	fake := fake.NewFakeProvider(toProviderScript(script))
	engine := review.Engine{
		Provider: fake,
		Budget:   review.Budget{},
		Policy:   review.DefaultPolicy(),
	}

	report, err := engine.Run(context.Background(), sessionID, snap, review.RunOptions{})
	if err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))

	fmt.Fprintf(os.Stderr, "\nprovider calls: %d\n", len(fake.Calls()))
	return nil
}

func toProviderScript(in scriptFile) map[domain.Role][]fake.ScriptedCall {
	out := make(map[domain.Role][]fake.ScriptedCall, len(in))
	for role, calls := range in {
		converted := make([]fake.ScriptedCall, 0, len(calls))
		for _, c := range calls {
			converted = append(converted, fake.ScriptedCall{
				Body:           c.Body,
				TransportError: c.TransportError,
				Message:        c.Message,
			})
		}
		out[role] = converted
	}
	return out
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// demoSnapshot is a tiny design used by the built-in demonstration.
func demoSnapshot() snapshotFile {
	return snapshotFile{
		ID: "snap-demo",
		Units: []evidence.Unit{
			{ID: "BRIEF-1", Kind: evidence.UnitBrief, Text: "Review a campus room booking design before implementation."},
			{ID: "REQ-1", Kind: evidence.UnitRequirement, Text: "A student may cancel their own booking."},
			{ID: "REQ-2", Kind: evidence.UnitRequirement, Text: "A room may not be double-booked for the same slot."},
			{ID: "COMP-1", Kind: evidence.UnitComponent, Text: "Booking service owns all booking writes."},
			{ID: "FLOW-1", Kind: evidence.UnitFlow, Text: "The client posts a booking request to the booking service."},
			{ID: "DATA-1", Kind: evidence.UnitDataRule, Text: "A booking records the owning student id."},
		},
	}
}

// demoScript gives every role one valid response. The QA role is scripted to
// fail its first response and succeed on the repair, so the demonstration also
// exercises the second-call path.
func demoScript() scriptFile {
	valid := `{"findings":[{"id":"F-1","severity":"medium","category":"ownership","issue":"Ownership is asserted but not enforced anywhere in the design.","recommendation":"State which component checks that the caller owns the booking before a cancel is accepted.","basis_refs":["REQ-1","DATA-1"]}]}`

	out := make(scriptFile, domain.RoleCount)
	for _, role := range domain.Roles {
		out[role] = []scriptedCall{{Body: valid}}
	}
	out[domain.RoleQA] = []scriptedCall{
		{Body: "sorry, I could not produce JSON"},
		{Body: valid},
	}
	return out
}
