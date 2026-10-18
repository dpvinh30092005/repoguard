package secrets

import (
	"context"
	"strings"
	"testing"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

func TestScanLine(t *testing.T) {
	// Fixtures are assembled at runtime so this file itself doesn't trip scanners.
	aws := "AKIA" + "IOSFODNN7REALKEY"
	gh := "ghp_" + strings.Repeat("a1B2", 9)
	cases := []struct {
		line string
		want string // rule id, "" = no finding
	}{
		{`key := "` + aws + `"`, "aws-access-key"},
		{`apiKey = "` + aws + `"`, "aws-access-key"}, // not also reported as generic-secret
		{`token: ` + gh, "github-token"},
		{`-----BEGIN RSA PRIVATE KEY-----`, "private-key"},   // repoguard:allow (fixture)
		{`DB_PASSWORD="Zx8#qLm2!vR9tYp4"`, "generic-secret"}, // repoguard:allow (fixture)
		{`password = "changeme-changeme"`, ""},               // placeholder
		{`api_key: "${API_KEY_FROM_ENV}"`, ""},               // templated
		{`password = "aaaaaaaaaaaaaaaa"`, ""},                // low entropy
		{`key := "` + aws + `" // repoguard:allow`, ""},      // explicit allow
		{`color: #ffffff`, ""},
	}
	for _, c := range cases {
		got := ScanLine(c.line)
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%q: unexpected %+v", c.line, got)
		case c.want != "" && (len(got) != 1 || got[0].Rule != c.want):
			t.Errorf("%q: got %+v, want %s", c.line, got, c.want)
		}
	}
}

func TestMaskNeverLeaksSecret(t *testing.T) {
	secret := "AKIA" + "IOSFODNN7REALKEY"
	m := ScanLine(secret)
	if len(m) != 1 || strings.Contains(m[0].Masked, "IOSFODNN7REALKEY") {
		t.Fatalf("got %+v", m)
	}
}

func TestCheckOnlyLooksAtAddedLinesAndHonoursIgnore(t *testing.T) {
	aws := "AKIA" + "IOSFODNN7REALKEY"
	files := []platform.FileChange{
		{Path: "app/config.go", Status: "modified", Patch: "@@ -1,2 +1,2 @@\n-old := \"" + aws + "\"\n+key := \"" + aws + "\"\n ctx"},
		{Path: "pkg/testdata/fixture.txt", Status: "added", Patch: "@@ -0,0 +1 @@\n+" + aws},
	}
	res, err := Check{}.Run(context.Background(), checks.Input{Files: files, Config: config.Default()})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != platform.StateFailure || len(res.Findings) != 1 {
		t.Fatalf("got %+v", res)
	}
	if f := res.Findings[0]; f.Path != "app/config.go" || f.Line != 1 {
		t.Fatalf("finding anchored at %s:%d", f.Path, f.Line)
	}
}
