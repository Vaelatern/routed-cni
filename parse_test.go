package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestGocastURL(t *testing.T) {
	reg, err := gocastURL("http://10.0.0.1:9999", "announce", "10.0.11.33/32", map[string]string{
		"name":      "mockhttp",
		"community": "65534:2",
		"monitor":   "port:tcp:3000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reg != "http://10.0.0.1:9999/register?monitor=port%3Atcp%3A3000&name=mockhttp&vip=10.0.11.33%2F32&vip_communities=65534%3A2" {
		t.Fatalf("register url=%q", reg)
	}
	unreg, err := gocastURL("http://10.0.0.1:9999/", "withdraw", "10.0.11.33/32", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if unreg != "http://10.0.0.1:9999/unregister?name=10.0.11.33" {
		t.Fatalf("unregister url=%q", unreg)
	}
}

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

func TestNomadAPIDefaultsAndToken(t *testing.T) {
	hostIP := getHostIP()
	if hostIP == "" {
		t.Skip("no host IP")
	}

	var gotAuth string
	var gotPath string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Nomad-Token")
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Address": hostIP, "Port": 9999},
		})
	}))
	defer ts.Close()

	// empty token file → no header; nomadAddr from conf; TLS insecure
	conf := &NetConf{
		NomadAddr: ts.URL,
		NomadTLS:  &NomadTLS{Insecure: true},
	}
	url, err := resolveLocalService("gocast", conf)
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://"+hostIP+":9999" {
		t.Fatalf("url=%q", url)
	}
	if gotAuth != "" || gotPath != "/v1/service/gocast" {
		t.Fatalf("auth=%q path=%q", gotAuth, gotPath)
	}

	// token file set → header sent
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte(" secret \n"), 0600); err != nil {
		t.Fatal(err)
	}
	conf.NomadTokenFile = tok
	if _, err := resolveLocalService("gocast", conf); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "secret" {
		t.Fatalf("auth=%q", gotAuth)
	}

	// empty conf.NomadAddr defaults inside resolve — only exercised via client build
	if loadNomadToken("") != "" {
		t.Fatal("empty token file should yield no token")
	}
}
