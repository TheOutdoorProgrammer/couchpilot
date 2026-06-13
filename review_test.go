package main

import (
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

func TestDiffHunksLineNumbers(t *testing.T) {
	base := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\n"
	proposed := "l1\nl2\nl3\nl4\nCHANGED\nl6\nl7\nl8\nl9\nl10\n"

	hunks := diffHunks(base, proposed)
	if len(hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(hunks))
	}
	rows := hunks[0].Rows
	// 3 ctx + del + add + 3 ctx
	if len(rows) != 8 {
		t.Fatalf("want 8 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].T != "ctx" || rows[0].O != 2 || rows[0].N != 2 || rows[0].Text != "l2" {
		t.Errorf("first ctx row wrong: %+v", rows[0])
	}
	var del, add *DiffRow
	for i := range rows {
		switch rows[i].T {
		case "del":
			del = &rows[i]
		case "add":
			add = &rows[i]
		}
	}
	if del == nil || del.O != 5 || del.Text != "l5" {
		t.Errorf("del row wrong: %+v", del)
	}
	if add == nil || add.N != 5 || add.Text != "CHANGED" {
		t.Errorf("add row wrong: %+v", add)
	}
}

func TestDiffHunksSeparation(t *testing.T) {
	// Two edits far apart must produce two hunks.
	var b, p strings.Builder
	for i := 1; i <= 40; i++ {
		b.WriteString(line(i) + "\n")
		if i == 5 {
			p.WriteString("EDIT-A\n")
		} else if i == 35 {
			p.WriteString("EDIT-B\n")
		} else {
			p.WriteString(line(i) + "\n")
		}
	}
	hunks := diffHunks(b.String(), p.String())
	if len(hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d", len(hunks))
	}
}

func TestDiffHunksNewFile(t *testing.T) {
	hunks := diffHunks("", "one\ntwo\n")
	if len(hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(hunks))
	}
	for i, r := range hunks[0].Rows {
		if r.T != "add" || r.N != i+1 {
			t.Errorf("row %d: %+v", i, r)
		}
	}
	adds, dels := diffStats("", "one\ntwo\n")
	if adds != 2 || dels != 0 {
		t.Errorf("stats: %d/%d", adds, dels)
	}
}

func TestDiffHunksNoChange(t *testing.T) {
	if h := diffHunks("same\n", "same\n"); h != nil {
		t.Errorf("want nil hunks, got %+v", h)
	}
}

func line(i int) string {
	return "line-" + strings.Repeat("x", i%3) + "-" + string(rune('a'+i%26))
}
