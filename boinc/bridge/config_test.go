// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "bridge.json")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	good := `{"gateway": "https://api.example.org/", "connector": "boinc", "key_file": "/run/bridge/key",
		"pilot_gateway": "wss://api.example.org/v1/node/connect", "project_dir": "/home/boincadm/project", "batch": 1,
		"model": {"url": "https://example.org/m.gguf", "nbytes": 14252845984, "md5": "` + strings.Repeat("0", 32) + `"},
		"web_url": "http://apache/project"}`
	c, err := LoadConfig(write(good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway != "https://api.example.org" || c.App != "pilot" || c.WUTemplate != "templates/pilot_in" ||
		c.CreditDir != "/home/boincadm/project/credit" || c.WebURL != "http://apache/project/" || c.DeadlineHours != 60 ||
		c.MinRunwayHours != 54 || c.PollSeconds != 10 || c.DemandSeconds != 60 {
		t.Errorf("defaults: %+v", c)
	}
	for _, bad := range []struct{ from, to string }{
		{`"batch": 1`, `"batch": 0`},
		{`"connector": "boinc"`, `"connector": "BOINC!"`},
		{`wss://api.example.org/v1/node/connect`, `wss://api.example.org/other`},
		{`"project_dir": "/home/boincadm/project"`, `"project_dir": "project"`},
		{`"md5": "`, `"md5": "x`},
		{`https://example.org/m.gguf`, `http://example.org/m.gguf`},
		{`"batch": 1`, `"batch": 1, "unknown": true`},
		{`"batch": 1`, `"batch": 1, "wu_template": "../x"`},
		{`"batch": 1`, `"batch": 1, "deadline_hours": 50`},
	} {
		if !strings.Contains(good, bad.from) {
			t.Fatalf("test: %q not in the config", bad.from)
		}
		if _, err := LoadConfig(write(strings.Replace(good, bad.from, bad.to, 1))); err == nil {
			t.Errorf("accepted %s", bad.to)
		}
	}
}

func TestProjectDB(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.xml"), []byte(`<boinc><config>
  <db_name>aisathome</db_name>
  <db_host>mysql</db_host>
  <db_user>root</db_user>
  <db_passwd>p@ss&amp;/word</db_passwd>
</config></boinc>`), 0o600)
	d, err := projectDB(dir)
	if err != nil || d != (dbSettings{Host: "mysql:3306", Name: "aisathome", User: "root", Passwd: "p@ss&/word"}) {
		t.Errorf("%+v %v", d, err)
	}
	if err := readSecretCheck(t, "two\nlines\n"); err == nil {
		t.Error("a two-line secret file")
	}
	if err := readSecretCheck(t, "  one  \n"); err != nil {
		t.Error(err)
	}
}

func readSecretCheck(t *testing.T, s string) error {
	p := filepath.Join(t.TempDir(), "s")
	os.WriteFile(p, []byte(s), 0o600)
	_, err := ReadSecret(p)
	return err
}
