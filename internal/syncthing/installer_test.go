package syncthing

import "testing"

func TestChecksumForArchive(t *testing.T) {
	checksums := "abc123  syncthing-v1.2.3-linux-amd64.tar.gz\ndef456  other.tar.gz\n"
	got, err := checksumForArchive(checksums, "syncthing-v1.2.3-linux-amd64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc123" {
		t.Fatalf("unexpected checksum %q", got)
	}
}

func TestChecksumForArchiveNotFound(t *testing.T) {
	if _, err := checksumForArchive("abc foo", "missing"); err == nil {
		t.Fatal("expected error")
	}
}
