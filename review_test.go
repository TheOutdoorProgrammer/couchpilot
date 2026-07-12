package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestApplyEdit(t *testing.T) {
	cases := []struct {
		name           string
		base, old, new string
		replaceAll     bool
		want           string
		wantMiss       bool
	}{
		{name: "single", base: "a\nb\na\n", old: "b", new: "X", want: "a\nX\na\n"},
		{name: "first only", base: "a\na\n", old: "a", new: "X", want: "X\na\n"},
		{name: "replace all", base: "a\na\n", old: "a", new: "X", replaceAll: true, want: "X\nX\n"},
		{name: "no match", base: "a\n", old: "zzz", new: "X", want: "a\n", wantMiss: true},
		{name: "empty old", base: "a\n", old: "", new: "X", want: "a\n", wantMiss: true},
	}
	for _, c := range cases {
		got, miss := applyEdit(c.base, c.old, c.new, c.replaceAll)
		if got != c.want || miss != c.wantMiss {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, miss, c.want, c.wantMiss)
		}
	}
}

// numberedLines builds "l1\nl2\n...\lN\n", the predictable input the segment
// tests diff against.
func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "l%d\n", i)
	}
	return b.String()
}

// flatten concatenates every segment's rows back into a single stream.
func flatten(segs []DiffSegment) []DiffRow {
	var rows []DiffRow
	for _, s := range segs {
		rows = append(rows, s.Rows...)
	}
	return rows
}

func kinds(segs []DiffSegment) []string {
	ks := make([]string, len(segs))
	for i, s := range segs {
		ks[i] = s.Kind
	}
	return ks
}

func findRow(rows []DiffRow, t string, text string) *DiffRow {
	for i := range rows {
		if rows[i].T == t && rows[i].Text == text {
			return &rows[i]
		}
	}
	return nil
}

// TestDiffSegmentsSingleChange: a change in a 20-line file leaves big unchanged
// runs on both sides, so the diff is a gap / hunk / gap sandwich.
func TestDiffSegmentsSingleChange(t *testing.T) {
	base := numberedLines(20)
	proposed := strings.Replace(base, "l10\n", "CHANGED\n", 1)

	segs := diffSegments(base, proposed)
	if got := kinds(segs); !reflect.DeepEqual(got, []string{"gap", "hunk", "gap"}) {
		t.Fatalf("kinds = %v, want [gap hunk gap]", got)
	}

	lead := segs[0].Rows
	if lead[0].T != "ctx" || lead[0].O != 1 || lead[0].N != 1 || lead[0].Text != "l1" {
		t.Errorf("lead gap first row wrong: %+v", lead[0])
	}

	hunk := segs[1].Rows
	if del := findRow(hunk, "del", "l10"); del == nil || del.O != 10 {
		t.Errorf("hunk del row wrong: %+v", del)
	}
	if add := findRow(hunk, "add", "CHANGED"); add == nil || add.N != 10 {
		t.Errorf("hunk add row wrong: %+v", add)
	}

	tail := segs[2].Rows
	if tail[0].T != "ctx" || tail[0].O != 14 {
		t.Errorf("tail gap first row wrong: %+v", tail[0])
	}
	for _, r := range tail {
		if r.T != "ctx" {
			t.Errorf("gap rows must all be ctx, got %+v", r)
		}
	}
}

// TestDiffSegmentsDistantChanges: two edits far apart yield hunk / gap / hunk,
// with the unchanged middle preserved as the gap's rows.
func TestDiffSegmentsDistantChanges(t *testing.T) {
	base := numberedLines(40)
	proposed := strings.Replace(base, "l5\n", "EDIT-A\n", 1)
	proposed = strings.Replace(proposed, "l35\n", "EDIT-B\n", 1)

	segs := diffSegments(base, proposed)
	if got := kinds(segs); !reflect.DeepEqual(got, []string{"hunk", "gap", "hunk"}) {
		t.Fatalf("kinds = %v, want [hunk gap hunk]", got)
	}
	gap := segs[1].Rows
	if gap[0].T != "ctx" || gap[0].O != 9 {
		t.Errorf("gap should start at l9: %+v", gap[0])
	}
	if findRow(segs[0].Rows, "del", "l5") == nil || findRow(segs[2].Rows, "del", "l35") == nil {
		t.Error("each hunk should carry its own change")
	}
}

// TestDiffSegmentsShortGapMerged: when two changes are close enough that the
// stretch between them is <= gapMinCollapse, it folds into one hunk instead of
// becoming a (noisy) one-line expand bar.
func TestDiffSegmentsShortGapMerged(t *testing.T) {
	base := numberedLines(30)
	proposed := strings.Replace(base, "l10\n", "EDIT-A\n", 1)
	proposed = strings.Replace(proposed, "l18\n", "EDIT-B\n", 1)

	segs := diffSegments(base, proposed)
	hunks := 0
	var theHunk []DiffRow
	for _, s := range segs {
		if s.Kind == "hunk" {
			hunks++
			theHunk = s.Rows
		}
	}
	if hunks != 1 {
		t.Fatalf("want the two close changes merged into 1 hunk, got %d hunks (%v)", hunks, kinds(segs))
	}
	if findRow(theHunk, "del", "l10") == nil || findRow(theHunk, "del", "l18") == nil {
		t.Error("merged hunk should contain both changes")
	}
}

func TestDiffSegmentsNewFile(t *testing.T) {
	segs := diffSegments("", "one\ntwo\n")
	if got := kinds(segs); !reflect.DeepEqual(got, []string{"hunk"}) {
		t.Fatalf("kinds = %v, want [hunk]", got)
	}
	for i, r := range segs[0].Rows {
		if r.T != "add" || r.N != i+1 {
			t.Errorf("row %d: %+v", i, r)
		}
	}
	if adds, dels := diffStats("", "one\ntwo\n"); adds != 2 || dels != 0 {
		t.Errorf("stats: %d/%d", adds, dels)
	}
}

func TestDiffSegmentsNoChange(t *testing.T) {
	if segs := diffSegments("same\n", "same\n"); segs != nil {
		t.Errorf("want nil segments, got %+v", segs)
	}
}

// TestDiffSegmentsReconstruct: segmenting must not drop or reorder rows — the
// flattened stream's old/new line numbers stay a contiguous 1..N sequence.
func TestDiffSegmentsReconstruct(t *testing.T) {
	base := numberedLines(25)
	proposed := strings.Replace(base, "l13\n", "CHANGED\n", 1)

	rows := flatten(diffSegments(base, proposed))
	wantO, wantN := 1, 1
	for _, r := range rows {
		if r.T == "del" || r.T == "ctx" {
			if r.O != wantO {
				t.Errorf("old line numbering broke: row %+v want O=%d", r, wantO)
			}
			wantO++
		}
		if r.T == "add" || r.T == "ctx" {
			if r.N != wantN {
				t.Errorf("new line numbering broke: row %+v want N=%d", r, wantN)
			}
			wantN++
		}
	}
	if wantO != 26 || wantN != 26 {
		t.Errorf("expected 25 old and 25 new lines, got %d/%d", wantO-1, wantN-1)
	}
}

// TestRangeCommentFormatting: a range quotes only its first line and cites the
// span (which tells claude the last line); single-line comments are unchanged.
func TestRangeCommentFormatting(t *testing.T) {
	r := &Review{FilePath: "/tmp/f.go", Status: ReviewDenied, Comments: []ReviewComment{
		{Line: 3, Side: "new", LineText: "single line", Text: "single note"},
		{StartLine: 5, StartSide: "new", StartText: "foo := bar()", Line: 8, Side: "new", LineText: "return foo", Text: "range note"},
	}}
	var b strings.Builder
	writeComments(&b, r)
	out := b.String()
	for _, want := range []string{
		"Line 3 (new): `single line`",
		"single note",
		"Lines 5-8 (new)",
		"first line `foo := bar()`",
		"range note",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatting missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "`return foo`") {
		t.Errorf("range must not quote the last line:\n%s", out)
	}
}

// TestRangeCommentCrossSide: a span from a deleted row to an added row spells out
// each endpoint's side.
func TestRangeCommentCrossSide(t *testing.T) {
	r := &Review{Status: ReviewDenied, Comments: []ReviewComment{
		{StartLine: 5, StartSide: "old", StartText: "gone", Line: 7, Side: "new", LineText: "added", Text: "crosses"},
	}}
	var b strings.Builder
	writeComments(&b, r)
	if out := b.String(); !strings.Contains(out, "Lines 5 (old/removed) to 7 (new)") {
		t.Errorf("cross-side label wrong:\n%s", out)
	}
}
