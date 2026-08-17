package cli

import (
	"strings"
	"testing"
)

// A user (or malware) editing config.xml to rejoin the public relay pool must
// not survive the next start.
func TestTamperedRelayListenAddressIsStripped(t *testing.T) {
	tampered := `<configuration version="37">
    <options>
        <listenAddress>default</listenAddress>
        <listenAddress>dynamic+https://relays.syncthing.net/endpoint</listenAddress>
        <listenAddress>tcp://0.0.0.0:22000</listenAddress>
        <globalAnnounceEnabled>true</globalAnnounceEnabled>
    </options>
</configuration>`
	out, changed, err := rewriteLocalSyncthingConfigXML([]byte(tampered))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a tampered listen address must be rewritten")
	}
	got := string(out)
	for _, forbidden := range []string{"relays.syncthing.net", "0.0.0.0:22000", "<listenAddress>default</listenAddress>"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("%q survived the rewrite:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, "tcp://127.0.0.1:0") || !strings.Contains(got, "quic://127.0.0.1:0") {
		t.Fatalf("expected loopback-only listeners:\n%s", got)
	}
}

// Same for the options okdev pins: whatever the file says, these are forced.
func TestTamperedNetworkOptionsAreForcedBack(t *testing.T) {
	cfg := map[string]any{"options": map[string]any{
		"relaysEnabled":         true,
		"globalAnnounceEnabled": true,
		"localAnnounceEnabled":  true,
		"natEnabled":            true,
		"urAccepted":            1,
		"crashReportingEnabled": true,
		"autoUpgradeIntervalH":  12,
	}}
	applyManagedSyncthingGlobalDefaults(cfg, false)
	opts := cfg["options"].(map[string]any)
	for k, want := range map[string]any{
		"relaysEnabled":         false,
		"globalAnnounceEnabled": false,
		"localAnnounceEnabled":  false,
		"natEnabled":            false,
		"urAccepted":            -1,
		"crashReportingEnabled": false,
		"autoUpgradeIntervalH":  0,
	} {
		if opts[k] != want {
			t.Errorf("%s = %#v, want %#v", k, opts[k], want)
		}
	}
}
