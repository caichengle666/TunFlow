//go:build windows

package main

import "testing"

func TestPrefixToMask(t *testing.T) {
	tests := map[int]string{
		0:  "0.0.0.0",
		8:  "255.0.0.0",
		15: "255.254.0.0",
		24: "255.255.255.0",
		32: "255.255.255.255",
	}
	for prefix, want := range tests {
		if got := prefixToMask(prefix); got != want {
			t.Fatalf("prefixToMask(%d) = %q, want %q", prefix, got, want)
		}
	}
	if got := prefixToMask(-1); got != "" {
		t.Fatalf("prefixToMask(-1) = %q, want empty", got)
	}
	if got := prefixToMask(33); got != "" {
		t.Fatalf("prefixToMask(33) = %q, want empty", got)
	}
}

func TestSameStringSet(t *testing.T) {
	if !sameStringSet([]string{"2", "1"}, []string{"1", "2"}) {
		t.Fatal("expected sets with different ordering to match")
	}
	if sameStringSet([]string{"1"}, []string{"1", "2"}) {
		t.Fatal("expected sets with different lengths not to match")
	}
	if sameStringSet([]string{"1"}, []string{"2"}) {
		t.Fatal("expected different sets not to match")
	}
}
