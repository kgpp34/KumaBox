package network

import (
	"reflect"
	"testing"
)

func TestNftNATRuleHandlesSelectsOnlyRequestedCIDR(t *testing.T) {
	t.Parallel()

	output := []byte(`table inet kumabox {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr 10.20.0.0/24 masquerade comment "first" # handle 7
		ip saddr 10.30.0.0/24 masquerade # handle 9
		ip daddr 10.20.0.0/24 masquerade # handle 11
	}
}`)

	if got, want := nftNATRuleHandles(output, "10.20.0.0/24"), []string{"7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("nftNATRuleHandles() = %v, want %v", got, want)
	}
}

func TestNftNATRuleHandlesReturnsAllDuplicateHandles(t *testing.T) {
	t.Parallel()

	output := []byte("ip saddr 10.20.0.0/24 masquerade # handle 4\n" +
		"ip saddr 10.20.0.0/24 masquerade # handle 5\n")
	if got, want := nftNATRuleHandles(output, "10.20.0.0/24"), []string{"4", "5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("nftNATRuleHandles() = %v, want %v", got, want)
	}
}
