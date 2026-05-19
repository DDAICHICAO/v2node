package core

import (
	"reflect"
	"testing"
)

func TestNormalizeRouteDomainMatches(t *testing.T) {
	got := normalizeRouteDomainMatches([]string{
		"https://Example.COM/path?a=1",
		"example.com",
		"*.Ads.Example.com:443",
		"regex:.*\\.tracker\\.com",
		"geosite:category-ads-all",
		"",
	})
	want := []string{
		"example.com",
		"domain:ads.example.com",
		"regexp:.*\\.tracker\\.com",
		"geosite:category-ads-all",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeRouteDomainMatches() = %#v, want %#v", got, want)
	}
}
