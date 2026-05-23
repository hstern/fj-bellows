package forgejo

import (
	"reflect"
	"testing"
)

// Package-level constants for the repeated label literals across these tests
// (goconst flags them otherwise; values match the issue's reproducers).
const (
	labelUbuntu = "ubuntu-latest"
	labelWeird  = "weird:value-with-colon"
)

func TestSplitLabel(t *testing.T) {
	cases := []struct {
		in          string
		wantName    string
		wantBinding string
	}{
		{
			in:          labelUbuntu,
			wantName:    labelUbuntu,
			wantBinding: "",
		},
		{
			// The de-facto Forgejo Actions binding from #39.
			in:          "ubuntu-latest:docker://catthehacker/ubuntu:act-latest",
			wantName:    labelUbuntu,
			wantBinding: "docker://catthehacker/ubuntu:act-latest",
		},
		{
			in:          "self-hosted:host://default",
			wantName:    "self-hosted",
			wantBinding: "host://default",
		},
		{
			in:          "k8s:kubernetes://default/pool",
			wantName:    "k8s",
			wantBinding: "kubernetes://default/pool",
		},
		{
			// Colon in the value but no recognised scheme → treat as bare so we
			// don't silently strip a name an operator intended (e.g. a custom
			// label that happens to contain ":1.0").
			in:          labelWeird,
			wantName:    labelWeird,
			wantBinding: "",
		},
		{
			in:          "",
			wantName:    "",
			wantBinding: "",
		},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			gotName, gotBinding := SplitLabel(c.in)
			if gotName != c.wantName || gotBinding != c.wantBinding {
				t.Errorf("SplitLabel(%q) = (%q, %q); want (%q, %q)",
					c.in, gotName, gotBinding, c.wantName, c.wantBinding)
			}
		})
	}
}

func TestBareLabels(t *testing.T) {
	in := []string{
		"ubuntu-latest:docker://catthehacker/ubuntu:act-latest",
		"self-hosted",
		"k8s:kubernetes://default/pool",
		labelWeird,
	}
	want := []string{labelUbuntu, "self-hosted", "k8s", labelWeird}
	got := BareLabels(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BareLabels(%v)\n  got:  %v\n  want: %v", in, got, want)
	}
}

func TestBareLabelsPreservesLengthForEmpty(t *testing.T) {
	if got := BareLabels(nil); len(got) != 0 {
		t.Errorf("BareLabels(nil) = %v, want empty", got)
	}
	if got := BareLabels([]string{}); len(got) != 0 {
		t.Errorf("BareLabels([]) = %v, want empty", got)
	}
}
