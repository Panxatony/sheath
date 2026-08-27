package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The seed holds the token and the serial as well. Rewriting one line of it
// must not be the thing that loses them.
func TestReplaceEnvValueKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.env")
	const before = "# placed by sheath-installer during provisioning\n" +
		"SHEATH_SERVER=http://10.0.0.10:8080\nSHEATH_SERIAL=10000000deadbeef\n" +
		"SHEATH_TOKEN=secret\nSHEATH_IMAGE=ubuntu-24.04-arm64\n"
	if err := os.WriteFile(p, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceEnvValue(p, serverKey, "http://10.0.1.10:8081"); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"SHEATH_SERVER=http://10.0.1.10:8081",
		"SHEATH_SERIAL=10000000deadbeef",
		"SHEATH_TOKEN=secret",
		"SHEATH_IMAGE=ubuntu-24.04-arm64",
	} {
		if !contains(got, want) {
			t.Errorf("missing after the rewrite: %s\n%s", want, got)
		}
	}
	if contains(got, "10.0.0.10") {
		t.Error("the old address is still in the file")
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("the seed lost its mode: %v", st.Mode().Perm())
	}
}

// An address that does not answer is a blade that goes quiet, and a quiet
// blade cannot be told to come back. So it is checked first.
func TestAgentOnlyMovesToAServerThatAnswers(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer good.Close()
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer silent.Close()

	dir := t.TempDir()
	p := filepath.Join(dir, "agent.env")
	write := func() {
		if err := os.WriteFile(p, []byte("SHEATH_SERVER=http://old:8080\nSHEATH_TOKEN=t\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write()
	a := &agent{cfg: Config{Server: "http://old:8080"}, envFile: p}
	if change := a.adoptServer(map[string]any{"server_url": silent.URL}, p); change != "" {
		t.Errorf("moved to a server that answers 500: %q", change)
	}
	if a.cfg.Server != "http://old:8080" {
		t.Errorf("the address changed anyway: %s", a.cfg.Server)
	}

	write()
	a = &agent{cfg: Config{Server: "http://old:8080"}, envFile: p}
	if change := a.adoptServer(map[string]any{"server_url": good.URL}, p); change == "" {
		t.Error("did not move to a server that answers")
	}
	if a.cfg.Server != good.URL {
		t.Errorf("still talking to %s", a.cfg.Server)
	}
	out, _ := os.ReadFile(p)
	if !contains(string(out), "SHEATH_SERVER="+good.URL) {
		t.Errorf("the seed was not rewritten:\n%s", out)
	}

	// The same address twice is not a move.
	if change := a.adoptServer(map[string]any{"server_url": good.URL}, p); change != "" {
		t.Errorf("moved to where it already was: %q", change)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
