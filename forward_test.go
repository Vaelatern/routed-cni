package main

import "testing"

func TestForwardRules(t *testing.T) {
	got := forwardRules("10.0.11.33")
	if len(got) != 2 {
		t.Fatalf("want 2 rules, got %d", len(got))
	}
	if got[0][0] != "-d" || got[0][1] != "10.0.11.33/32" || got[0][3] != "ACCEPT" {
		t.Fatalf("dst rule: %v", got[0])
	}
	if got[1][0] != "-s" || got[1][1] != "10.0.11.33/32" || got[1][3] != "ACCEPT" {
		t.Fatalf("src rule: %v", got[1])
	}
}
