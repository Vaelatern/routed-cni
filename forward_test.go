package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

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

func TestAllocateBridgeIP(t *testing.T) {
	dir := t.TempDir()
	dataDir = dir
	_, subnet, err := net.ParseCIDR("172.27.64.0/30")
	if err != nil {
		t.Fatal(err)
	}
	gw := gatewayIP(subnet) // .1
	ip1, err := allocateBridgeIP(subnet, gw, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !ip1.Equal(net.ParseIP("172.27.64.2")) {
		t.Fatalf("want .2, got %s", ip1)
	}
	ip1b, err := allocateBridgeIP(subnet, gw, "c1")
	if err != nil || !ip1b.Equal(ip1) {
		t.Fatalf("idempotent: %v %v", ip1b, err)
	}
	_, err = allocateBridgeIP(subnet, gw, "c2")
	if err == nil {
		t.Fatal("expected subnet exhausted")
	}
	releaseBridgeIP(ip1.String(), "c1")
	ip2, err := allocateBridgeIP(subnet, gw, "c2")
	if err != nil || !ip2.Equal(ip1) {
		t.Fatalf("reuse after release: %v %v", ip2, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "by-id", "c2")); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayIP(t *testing.T) {
	_, n, _ := net.ParseCIDR("172.27.64.0/20")
	if g := gatewayIP(n); !g.Equal(net.ParseIP("172.27.64.1")) {
		t.Fatalf("got %s", g)
	}
}
