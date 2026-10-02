package bridge

import (
	"os"
	"runtime"
	"testing"
)

func TestLegacyPeerIdentityRetainsExactStartAndDomain(t *testing.T) {
	start, err := processStart(os.Getpid())
	must(t, err)
	must(t, verifyPeerIdentity(os.Getpid(), start, runtime.GOOS))
	for _, domain := range []string{"", "unknown", runtime.GOOS + ":unknown"} {
		if err := verifyPeerIdentity(os.Getpid(), start, domain); err == nil {
			t.Fatalf("legacy peer accepted domain %q", domain)
		}
	}
	if err := verifyPeerIdentity(os.Getpid(), "different process", runtime.GOOS); err == nil {
		t.Fatal("legacy peer accepted stale process identity")
	}
	for _, domain := range []string{"", runtime.GOOS} {
		target := Peer{PID: os.Getpid(), Start: start, Domain: domain}
		if !peerReceiptMatches(target, target.PID, start) {
			t.Fatal("legacy stored receipt identity was rejected")
		}
		if peerReceiptMatches(target, target.PID, "different process") {
			t.Fatal("legacy receipt accepted different observed identity")
		}
	}
}
