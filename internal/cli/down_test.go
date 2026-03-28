package cli

import "testing"

func TestNewDownCmdDeprecatesDeletePVC(t *testing.T) {
	cmd := newDownCmd(&Options{})
	flag := cmd.Flags().Lookup("delete-pvc")
	if flag == nil {
		t.Fatal("expected delete-pvc flag")
	}
	if flag.Deprecated == "" {
		t.Fatal("expected delete-pvc to be marked deprecated")
	}
}
