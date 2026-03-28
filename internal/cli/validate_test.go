package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewValidateCmdReportsValidConfig(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, ".okdev.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: default
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newValidateCmd(&Options{ConfigPath: cfgPath})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("validate execute: %v", err)
	}
	if !strings.Contains(out.String(), "Config valid: "+cfgPath) {
		t.Fatalf("unexpected output %q", out.String())
	}
}
