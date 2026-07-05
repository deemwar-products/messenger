package service

import "strings"

import "testing"

// The launch command sources the secret env file and execs the hub — it must reference
// the env file by PATH (so secret VALUES stay out of the unit) and carry the bin/addr.
func TestLaunchCmd_SourcesEnvFileNeverEmbedsSecrets(t *testing.T) {
	c := Config{Bin: "/usr/local/bin/messenger", Addr: ":14310", Home: "/h", EnvFile: "/h/serve-token.env"}
	cmd := c.launchCmd()
	if !strings.Contains(cmd, "/h/serve-token.env") {
		t.Fatalf("launch cmd must source the env file: %q", cmd)
	}
	if !strings.Contains(cmd, "exec \"/usr/local/bin/messenger\" serve --addr \":14310\"") {
		t.Fatalf("launch cmd must exec the hub: %q", cmd)
	}
	// No secret value can appear because we only ever reference the file, never read it.
	if strings.Contains(cmd, "TOKEN=") || strings.Contains(cmd, "SECRET=") {
		t.Fatalf("launch cmd must not embed any secret value: %q", cmd)
	}

	// With no env file, it just execs — no stray source.
	c2 := Config{Bin: "/b/messenger", Addr: ":1", Home: "/h"}
	if strings.Contains(c2.launchCmd(), "source") || strings.Contains(c2.launchCmd(), ". \"") {
		t.Fatalf("no env file → no source: %q", c2.launchCmd())
	}
}

func TestXMLEscape(t *testing.T) {
	if got := xmlEscape(`a & b < c > d`); got != "a &amp; b &lt; c &gt; d" {
		t.Fatalf("bad escape: %q", got)
	}
}
