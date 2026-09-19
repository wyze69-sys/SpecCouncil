package evidence

import "testing"

func units() []Unit {
	return []Unit{
		{ID: "R-1", Kind: UnitRequirement, Text: "Users can edit projects."},
		{ID: "C-1", Kind: UnitComponent, Text: "Project service owns writes."},
		{ID: "F-1", Kind: UnitFlow, Text: "The client calls the project service."},
	}
}

func TestFreezeComputesStableHashRegardlessOfInputOrder(t *testing.T) {
	a, err := Freeze("snap-1", units())
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}

	reordered := units()
	reordered[0], reordered[2] = reordered[2], reordered[0]
	b, err := Freeze("snap-1", reordered)
	if err != nil {
		t.Fatalf("Freeze reordered: %v", err)
	}

	if a.Hash != b.Hash {
		t.Errorf("hash depends on input order: %s vs %s", a.Hash, b.Hash)
	}
}

func TestFreezeIsImmutableAgainstCallerMutation(t *testing.T) {
	input := units()
	snap, err := Freeze("snap-1", input)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	before := snap.Hash

	input[0].Text = "tampered"
	if snap.Units[0].Text == "tampered" {
		t.Error("snapshot shares backing storage with the caller slice")
	}
	if snap.Hash != before {
		t.Error("snapshot hash changed after caller mutation")
	}
}

func TestFreezeRejectsBadInput(t *testing.T) {
	cases := map[string][]Unit{
		"empty list": []Unit{},
		"empty id":   {{ID: "  ", Kind: UnitRequirement, Text: "x"}},
		"empty text": {{ID: "R-1", Kind: UnitRequirement, Text: " "}},
		"bad kind":   {{ID: "R-1", Kind: "nonsense", Text: "x"}},
		"duplicate":  {{ID: "R-1", Kind: UnitRequirement, Text: "x"}, {ID: "R-1", Kind: UnitFlow, Text: "y"}},
	}
	for name, u := range cases {
		if _, err := Freeze("snap-1", u); err == nil {
			t.Errorf("%s: Freeze succeeded, want error", name)
		}
	}
	if _, err := Freeze("", units()); err == nil {
		t.Error("empty snapshot id: Freeze succeeded, want error")
	}
}

func TestHasRefUsesTheFrozenSet(t *testing.T) {
	snap, err := Freeze("snap-1", units())
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if !snap.HasRef("R-1") {
		t.Error("HasRef(R-1) = false, want true")
	}
	if snap.HasRef("R-999") {
		t.Error("HasRef(R-999) = true, want false: citations are never guessed")
	}
	if got := len(snap.Refs()); got != 3 {
		t.Errorf("Refs() has %d entries, want 3", got)
	}
}
