package core

import (
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestNativeBoundUsesFallbackAndClampsConfiguredValues(t *testing.T) {
	tests := []struct {
		name     string
		value    int
		fallback int
		low      int
		high     int
		want     int
	}{
		{name: "fallback", value: 0, fallback: 128, low: 1, high: 8192, want: 128},
		{name: "lower bound", value: -1, fallback: 128, low: 1, high: 8192, want: 1},
		{name: "configured", value: 256, fallback: 128, low: 1, high: 8192, want: 256},
		{name: "upper bound", value: 9000, fallback: 128, low: 1, high: 8192, want: 8192},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nativeBound(tt.value, tt.fallback, tt.low, tt.high); got != tt.want {
				t.Fatalf("nativeBound(%d, %d, %d, %d)=%d, want %d", tt.value, tt.fallback, tt.low, tt.high, got, tt.want)
			}
		})
	}
}

func TestNativeIngressBudgetRaisesBurstToRate(t *testing.T) {
	budget := newNativeIngressBudget(panel.EncSettings{
		NativePreauthRatePerIP:  100,
		NativePreauthBurstPerIP: 50,
	})

	if budget.preauthRate != 100 || budget.preauthBurst != 100 {
		t.Fatalf("preauth budget rate=%v burst=%v, want 100/100", budget.preauthRate, budget.preauthBurst)
	}
}

func TestNativeIngressBudgetTokenBucketRefillsPerIP(t *testing.T) {
	budget := newNativeIngressBudget(panel.EncSettings{
		NativePreauthRatePerIP:  2,
		NativePreauthBurstPerIP: 3,
	})
	now := time.Now()

	for i := 0; i < 3; i++ {
		if !budget.allowPreauth("198.51.100.10", now) {
			t.Fatalf("initial token %d was rejected", i+1)
		}
	}
	if budget.allowPreauth("198.51.100.10", now) {
		t.Fatal("burst should be exhausted for the source IP")
	}
	if !budget.allowPreauth("198.51.100.10", now.Add(500*time.Millisecond)) {
		t.Fatal("one token should refill after half a second")
	}
	if budget.allowPreauth("198.51.100.10", now.Add(500*time.Millisecond)) {
		t.Fatal("refilled token should be consumed once")
	}
	if !budget.allowPreauth("198.51.100.11", now) {
		t.Fatal("a different source IP must have an independent bucket")
	}
}

func TestSntpEclipseReplayCacheScopesAndExpiresEntries(t *testing.T) {
	cache := newSntpEclipseReplayCache(time.Minute)
	cache.capacity = 10
	cache.maxPerUser = 2
	cache.maxPerIP = 3
	now := time.Now()

	var first, second, third, fourth [16]byte
	first[0] = 1
	second[0] = 2
	third[0] = 3
	fourth[0] = 4

	if !cache.MarkScoped(first, 7, "198.51.100.20", now) {
		t.Fatal("first scoped replay entry was rejected")
	}
	if !cache.MarkScoped(second, 7, "198.51.100.20", now) {
		t.Fatal("second scoped replay entry was rejected")
	}
	if cache.MarkScoped(third, 7, "198.51.100.20", now) {
		t.Fatal("per-user replay quota was not enforced")
	}
	if !cache.MarkScoped(third, 8, "198.51.100.20", now) {
		t.Fatal("a different user should have an independent user quota")
	}
	if cache.MarkScoped(fourth, 9, "198.51.100.20", now) {
		t.Fatal("per-IP replay quota was not enforced")
	}

	expired := now.Add(2 * time.Minute)
	if !cache.MarkScoped(fourth, 9, "198.51.100.20", expired) {
		t.Fatal("expired scoped entries should release user and IP quota")
	}
	if cache.MarkScoped(fourth, 9, "198.51.100.20", expired) {
		t.Fatal("the same session ID must still be rejected as a replay")
	}
}

func TestSntpEclipseReplayCacheGlobalCapacityReleasesAfterExpiry(t *testing.T) {
	cache := newSntpEclipseReplayCache(time.Minute)
	cache.capacity = 2
	now := time.Now()

	var first, second, third [16]byte
	first[0] = 11
	second[0] = 12
	third[0] = 13

	if !cache.MarkScoped(first, 1, "198.51.100.30", now) || !cache.MarkScoped(second, 2, "198.51.100.31", now) {
		t.Fatal("initial entries were rejected")
	}
	if cache.MarkScoped(third, 3, "198.51.100.32", now) {
		t.Fatal("global replay capacity was not enforced")
	}
	if !cache.MarkScoped(third, 3, "198.51.100.32", now.Add(2*time.Minute)) {
		t.Fatal("expired entries did not release global replay capacity")
	}
}
