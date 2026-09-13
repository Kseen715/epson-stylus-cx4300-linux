package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func writeConf(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "escan.conf")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile respects umask, so set the mode we actually asked for.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigMissingFileIsNotAnError(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "absent.conf"))
	if err != nil || cfg != nil {
		t.Fatalf("got cfg=%v err=%v, want nil, nil", cfg, err)
	}
}

func TestConfigFillsOnlyUnsetFlags(t *testing.T) {
	path := writeConf(t, "addr = 0.0.0.0:9000\nscan-dpi = 600\n# comment\n\n", 0o600)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	fset := flag.NewFlagSet("escan", flag.ContinueOnError)
	addr := fset.String("addr", "127.0.0.1:8080", "")
	dpi := fset.Int("scan-dpi", 300, "")
	if err := fset.Parse([]string{"-addr", "127.0.0.1:1234"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.apply(fset, explicitFlags(fset)); err != nil {
		t.Fatal(err)
	}
	// The command line wins; the file supplies what it did not set.
	if *addr != "127.0.0.1:1234" {
		t.Errorf("addr = %q, want the command-line value", *addr)
	}
	if *dpi != 600 {
		t.Errorf("scan-dpi = %d, want 600 from the file", *dpi)
	}
}

func TestConfigRejectsUnknownAndMalformed(t *testing.T) {
	fset := flag.NewFlagSet("escan", flag.ContinueOnError)
	fset.String("addr", "", "")

	cfg, err := loadConfig(writeConf(t, "adress = x\n", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.apply(fset, nil); err == nil {
		t.Error("a misspelled setting must not be ignored")
	}
	if _, err := loadConfig(writeConf(t, "addr\n", 0o600)); err == nil {
		t.Error("a line with no = must be rejected")
	}
	if _, err := loadConfig(writeConf(t, "addr = a\naddr = b\n", 0o600)); err == nil {
		t.Error("a duplicated setting must be rejected")
	}
}

func TestPasswordIsConfigOnlyAndNeedsPrivateFile(t *testing.T) {
	fset := flag.NewFlagSet("escan", flag.ContinueOnError)
	fset.String("smb-user", "", "")

	// smb-password has no flag on purpose, and must not be reported unknown.
	cfg, err := loadConfig(writeConf(t, "smb-user = u\nsmb-password = pw#1 = x\n", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.apply(fset, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The value runs to the end of the line: # and = are legal in a password.
	if got := cfg.get("smb-password"); got != "pw#1 = x" {
		t.Errorf("password = %q, want it taken verbatim", got)
	}
	if err := cfg.checkSecret("smb-password"); err != nil {
		t.Errorf("mode 0600 must be accepted: %v", err)
	}

	loose, err := loadConfig(writeConf(t, "smb-password = pw\n", 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if err := loose.checkSecret("smb-password"); err == nil {
		t.Error("a world-readable file holding a password must be refused")
	}
}

func TestNewSMBStore(t *testing.T) {
	for _, tc := range []struct {
		address, want string
	}{
		{`//nas.lan/scans`, "//nas.lan/scans"},
		{`\\nas.lan\scans\cx4300`, "//nas.lan/scans/cx4300"},
		{`smb://nas.lan:4450/scans`, "//nas.lan:4450/scans"},
	} {
		st, err := newSMBStore(tc.address, "u", "p", "")
		if err != nil {
			t.Fatalf("%s: %v", tc.address, err)
		}
		if got := st.Describe(); got != tc.want {
			t.Errorf("%s: Describe = %q, want %q", tc.address, got, tc.want)
		}
	}

	st, err := newSMBStore("//nas/scans/sub", "u", "p", "")
	if err != nil {
		t.Fatal(err)
	}
	// SMB paths are backslash-separated on the wire.
	if got := st.remotePath("a.png"); got != `sub\a.png` {
		t.Errorf("remotePath = %q", got)
	}

	if _, err := newSMBStore("//nas", "u", "p", ""); err == nil {
		t.Error("an address with no share must be rejected")
	}
	if _, err := newSMBStore("//nas/scans", "", "p", ""); err == nil {
		t.Error("a share with no user must be rejected")
	}
}
