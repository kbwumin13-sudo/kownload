package pathpolicy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLayoutGoldenJSONAndFingerprint(t *testing.T) {
	in := []Member{{Index: 1, RelativePath: "sub/b.txt"}, {Index: 0, RelativePath: "a.txt", Selected: true}}
	layout, err := BuildLayoutV1("/Downloads", in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(layout)
	if err != nil {
		t.Fatal(err)
	}
	const expected = `{"version":1,"parentRoot":"/Downloads","members":[{"index":0,"relativePath":"a.txt","selected":true},{"index":1,"relativePath":"sub/b.txt","selected":false}]}`
	if string(raw) != expected {
		t.Fatalf("JSON = %s", raw)
	}
	fingerprint, err := LayoutFingerprint(layout)
	if err != nil {
		t.Fatal(err)
	}
	// Independently calculated from the exact UTF-8 JSON above.
	const golden = "8cc36d82236fd949a1d26445e810646e0904df2eb4340bc6dc17ef3a37296de3"
	if fingerprint != golden {
		t.Fatalf("fingerprint = %s", fingerprint)
	}
	var restored LayoutV1
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	got, err := LayoutFingerprint(restored)
	if err != nil || got != golden || !reflect.DeepEqual(restored, layout) {
		t.Fatalf("round trip = %+v, %q, %v", restored, got, err)
	}
}

func TestLayoutOrderAndSelectionTravelWithIndex(t *testing.T) {
	original := []Member{{Index: 0, RelativePath: "课/a.txt", Selected: true}, {Index: 1, RelativePath: "课/b.txt"}, {Index: 2, RelativePath: "课/c.txt", Selected: true}}
	var fingerprint string
	for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}, {1, 2, 0}, {2, 1, 0}} {
		in := []Member{original[order[0]], original[order[1]], original[order[2]]}
		before := append([]Member(nil), in...)
		// Fingerprinting decoded, unsorted values uses the same canonical order.
		got, err := LayoutFingerprint(LayoutV1{Version: 1, ParentRoot: "/Downloads", Members: in})
		if err != nil {
			t.Fatal(err)
		}
		if fingerprint != "" && fingerprint != got {
			t.Fatal("input order changed fingerprint")
		}
		fingerprint = got
		if !reflect.DeepEqual(before, in) {
			t.Fatal("fingerprinting sorted caller storage")
		}
		layout, err := BuildLayoutV1("/Downloads", in)
		if err != nil || !reflect.DeepEqual(layout.Members, original) {
			t.Fatalf("selection/index association changed: %+v %v", layout, err)
		}
	}
}

func TestLayoutOwnedCopy(t *testing.T) {
	in := []Member{{Index: 1, RelativePath: "b.txt"}, {Index: 0, RelativePath: "a.txt", Selected: true}}
	layout, err := BuildLayoutV1("/Downloads", in)
	if err != nil {
		t.Fatal(err)
	}
	in[0].RelativePath = "caller-mutated.txt"
	in[1].Selected = false
	if layout.Members[1].RelativePath != "b.txt" || !layout.Members[0].Selected {
		t.Fatal("built layout aliases caller members")
	}
	layout.Members[0].RelativePath = "result-mutated.txt"
	if in[1].RelativePath != "a.txt" {
		t.Fatal("caller members alias built layout")
	}
}

func TestLayoutPreservesPathBytes(t *testing.T) {
	parent := "/Downloads/../存档/"
	in := []Member{
		{Index: 0, RelativePath: "Café/A.txt"},
		{Index: 1, RelativePath: "Cafe\u0301/A.txt"},
		{Index: 2, RelativePath: "café/a.txt"},
		{Index: 3, RelativePath: "literal%2f%2e%2e.txt"},
	}
	layout, err := BuildLayoutV1(parent, in)
	if err != nil || layout.ParentRoot != parent || !reflect.DeepEqual(layout.Members, in) {
		t.Fatalf("path bytes changed: %+v %v", layout, err)
	}
	// Unicode/case aliases are not merged by this pure value helper. Actual
	// filesystem collisions and symlink containment require the IO owner.
	for _, pair := range [][2]string{{"Café.txt", "Cafe\u0301.txt"}, {"A.txt", "a.txt"}, {"a%2eb.txt", "a.b.txt"}} {
		first, _ := BuildLayoutV1(parent, []Member{{RelativePath: pair[0]}})
		second, _ := BuildLayoutV1(parent, []Member{{RelativePath: pair[1]}})
		a, _ := LayoutFingerprint(first)
		b, _ := LayoutFingerprint(second)
		if a == b {
			t.Fatalf("distinct byte layouts merged: %q / %q", pair[0], pair[1])
		}
	}
}

func TestLayoutRejectsUnsealedMembers(t *testing.T) {
	cases := []struct {
		name    string
		members []Member
	}{
		{"nil", nil}, {"empty", []Member{}},
		{"empty path", []Member{{}}},
		{"absolute", []Member{{RelativePath: "/a.txt"}}},
		{"parent", []Member{{RelativePath: "../a.txt"}}},
		{"embedded parent", []Member{{RelativePath: "a/../b.txt"}}},
		{"dot", []Member{{RelativePath: "."}}},
		{"embedded dot", []Member{{RelativePath: "a/./b.txt"}}},
		{"empty component", []Member{{RelativePath: "a//b.txt"}}},
		{"trailing slash", []Member{{RelativePath: "a/"}}},
		{"backslash", []Member{{RelativePath: `a\b.txt`}}},
		{"drive absolute", []Member{{RelativePath: "C:/a.txt"}}},
		{"drive relative", []Member{{RelativePath: "c:a.txt"}}},
		{"nul", []Member{{RelativePath: "a\x00.txt"}}},
		{"invalid UTF8", []Member{{RelativePath: "a\xff.txt"}}},
		{"negative index", []Member{{Index: -1, RelativePath: "a.txt"}}},
		{"gap", []Member{{Index: 0, RelativePath: "a.txt"}, {Index: 2, RelativePath: "b.txt"}}},
		{"duplicate index", []Member{{Index: 0, RelativePath: "a.txt"}, {Index: 0, RelativePath: "b.txt"}}},
		{"duplicate path", []Member{{Index: 0, RelativePath: "a.txt"}, {Index: 1, RelativePath: "a.txt", Selected: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]Member(nil), tc.members...)
			layout, err := BuildLayoutV1("/Downloads", tc.members)
			if !errors.Is(err, ErrLayout) || !reflect.DeepEqual(layout, LayoutV1{}) {
				t.Fatalf("nonzero failed layout: %+v %v", layout, err)
			}
			if len(tc.members) != len(before) || len(before) != 0 && !reflect.DeepEqual(before, tc.members) {
				t.Fatal("failed validation mutated members")
			}
			fingerprint, err := LayoutFingerprint(LayoutV1{Version: 1, ParentRoot: "/Downloads", Members: tc.members})
			if !errors.Is(err, ErrLayout) || fingerprint != "" {
				t.Fatalf("invalid layout fingerprint: %q %v", fingerprint, err)
			}
		})
	}
}

func TestLayoutRejectsInvalidRootAndVersion(t *testing.T) {
	members := []Member{{RelativePath: "a.txt"}}
	for _, parent := range []string{"", "relative", "/bad\x00", "/bad\xff"} {
		layout, err := BuildLayoutV1(parent, members)
		if !errors.Is(err, ErrLayout) || !reflect.DeepEqual(layout, LayoutV1{}) {
			t.Fatalf("invalid root accepted: %+v %v", layout, err)
		}
		got, err := LayoutFingerprint(LayoutV1{Version: 1, ParentRoot: parent, Members: members})
		if got != "" || !errors.Is(err, ErrLayout) {
			t.Fatalf("invalid root fingerprint: %q %v", got, err)
		}
	}
	for _, version := range []int{-1, 0, 2, 999} {
		in := LayoutV1{Version: version, ParentRoot: "/Downloads", Members: members}
		got, err := LayoutFingerprint(in)
		if got != "" || !errors.Is(err, ErrUnsupportedVersion) || in.Version != version {
			t.Fatalf("unknown version approved: %q %v", got, err)
		}
	}
}

func TestLayoutFingerprintBindsFinalLayoutOnly(t *testing.T) {
	base, _ := BuildLayoutV1("/Downloads", []Member{{RelativePath: "a.txt", Selected: true}})
	fingerprint, err := LayoutFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []LayoutV1{
		{Version: 1, ParentRoot: "/Other", Members: base.Members},
		{Version: 1, ParentRoot: base.ParentRoot, Members: []Member{{RelativePath: "b.txt", Selected: true}}},
		{Version: 1, ParentRoot: base.ParentRoot, Members: []Member{{RelativePath: "a.txt"}}},
	} {
		got, err := LayoutFingerprint(changed)
		if err != nil || got == fingerprint {
			t.Fatalf("changed layout not bound: %q %v", got, err)
		}
	}
	if len(fingerprint) != 64 || fingerprint != strings.ToLower(fingerprint) {
		t.Fatalf("not lowercase SHA-256: %q", fingerprint)
	}
}
