package main

import "testing"

// TestRetraction_LaterEntryCarriesRationaleFirstDoesNot reproduces a real,
// live-verified divergence: two separate `retract` directives both cover the
// checked version, the first (in file order) with no comment, the second
// with one. Real cmd/go's CheckRetractions (modload/modfile.go) walks every
// retract entry and collects the first *non-empty* rationale among every
// matching entry, not just whichever entry happens to match first — see
// retraction's doc comment for the live `go list -m -u -retracted` trace
// this was verified against. Before the fix, retraction() returned on the
// first matching entry outright (empty rationale here), silently dropping
// the real explanation the second entry carries.
func TestRetraction_LaterEntryCarriesRationaleFirstDoesNot(t *testing.T) {
	const modBody = `module example.com/retracttest

go 1.21

retract v1.2.3

retract [v1.0.0, v2.0.0] // big incident, avoid this whole range
`
	rationale, retracted := retraction(modBody, "v1.2.3")
	if !retracted {
		t.Fatal("expected v1.2.3 to be reported retracted (covered by the second, ranged entry)")
	}
	const want = "big incident, avoid this whole range"
	if rationale != want {
		t.Fatalf("rationale = %q, want %q (the second entry's, since the first matching entry's own rationale is empty)", rationale, want)
	}
}

// TestRetraction_FirstMatchingEntryRationaleWins confirms the ordinary case
// (the first matching entry already carries a rationale) is unaffected by
// widening the scan to every entry: real go's Rationale[0] is still that
// entry's text, not a later, different entry's, when the first match already
// has one.
func TestRetraction_FirstMatchingEntryRationaleWins(t *testing.T) {
	const modBody = `module example.com/retracttest

go 1.21

retract v1.2.3 // first, explained

retract [v1.0.0, v2.0.0] // second, also explained
`
	rationale, retracted := retraction(modBody, "v1.2.3")
	if !retracted {
		t.Fatal("expected v1.2.3 to be reported retracted")
	}
	const want = "first, explained"
	if rationale != want {
		t.Fatalf("rationale = %q, want %q (the first matching entry's own rationale)", rationale, want)
	}
}

// TestRetraction_NoMatchingEntryHasRationale confirms the existing, already-
// tested "no rationale anywhere" shape still returns an empty rationale
// (diagnose() falls back to the bare "retracted by module author" wording
// for this case), not a false-positive pull from an entry that doesn't even
// cover the checked version.
func TestRetraction_NoMatchingEntryHasRationale(t *testing.T) {
	const modBody = `module example.com/retracttest

go 1.21

retract v1.2.3

retract v9.9.9 // unrelated, different version entirely
`
	rationale, retracted := retraction(modBody, "v1.2.3")
	if !retracted {
		t.Fatal("expected v1.2.3 to be reported retracted")
	}
	if rationale != "" {
		t.Fatalf("rationale = %q, want empty (the only entry covering v1.2.3 has no rationale, and the other entry doesn't cover it at all)", rationale)
	}
}
