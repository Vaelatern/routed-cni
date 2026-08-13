package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

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
