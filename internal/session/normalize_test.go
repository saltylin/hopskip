package session

import "testing"

func TestNormalizeScreen(t *testing.T) {
	// the reported shape: a few real lines, then the pane padded with blank rows
	in := "penguin.local\n/Users/sumi/go/src\nsumi@penguin hopskip % \n\n\n\n\n\n\n\n"
	got := normalizeScreen(in)
	want := "penguin.local\n/Users/sumi/go/src\nsumi@penguin hopskip %"
	if got != want {
		t.Fatalf("trailing-blank trim:\n got %q\nwant %q", got, want)
	}

	// right-trim padding spaces + drop leading blanks + collapse internal runs
	in2 := "\n\nalpha   \nbeta\t\n\n\n\ngamma\n\n"
	got2 := normalizeScreen(in2)
	want2 := "alpha\nbeta\n\ngamma" // leading gone, trailing gone, 3-blank run → 1
	if got2 != want2 {
		t.Fatalf("collapse/trim:\n got %q\nwant %q", got2, want2)
	}

	// all-blank → empty
	if got := normalizeScreen("\n\n\n"); got != "" {
		t.Fatalf("all-blank should be empty, got %q", got)
	}

	// a single clean line is unchanged
	if got := normalizeScreen("hello"); got != "hello" {
		t.Fatalf("clean line changed: %q", got)
	}
}
