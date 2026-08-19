package node

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestManagedTLSDNS01PolicyForCloudflare(t *testing.T) {
	policy, ok := managedTLSDNS01PolicyForProvider(" CLOUDFLARE ")
	if !ok {
		t.Fatal("expected cloudflare managed TLS DNS-01 policy")
	}
	if policy.PropagationWait != 15*time.Second || !policy.SkipPropagationCheck {
		t.Fatalf("unexpected propagation policy: %#v", policy)
	}
	for _, nameserver := range []string{"1.1.1.1:53", "8.8.8.8:53"} {
		if !slices.Contains(policy.RecursiveNameservers, nameserver) {
			t.Fatalf("missing recursive nameserver %q: %#v", nameserver, policy)
		}
	}
}

func TestManagedTLSDNS01PolicyLeavesOtherProvidersUntouched(t *testing.T) {
	if _, ok := managedTLSDNS01PolicyForProvider("route53"); ok {
		t.Fatal("non-cloudflare providers must retain lego defaults")
	}
}

func TestManagedTLSObtainFailureKeepsStableMachineCode(t *testing.T) {
	err := managedTLSObtainFailure(3, errors.New("authoritative TXT missing"))
	if err == nil || err.Error() != "managed_tls_obtain_failed" {
		t.Fatalf("unexpected mapped error: %v", err)
	}
}
