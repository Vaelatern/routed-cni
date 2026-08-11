package main

import "testing"

func TestParseNomadService(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"service:nomad", "gocast", true},
		{"service:nomad:service:my-gocast", "my-gocast", true},
		{"http://127.0.0.1:8080", "", false},
		{"service:nomad:service:", "", false},
	}
	for _, c := range cases {
		got, ok := parseNomadService(c.in)
		if ok != c.wantOK || got != c.wantName {
			t.Fatalf("parseNomadService(%q)=(%q,%v) want (%q,%v)", c.in, got, ok, c.wantName, c.wantOK)
		}
	}
}
