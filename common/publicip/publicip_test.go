package publicip

import "testing"

func TestNormalizeAcceptsOnlyPublicIPs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "public ipv4", in: " 8.8.8.8\n", want: "8.8.8.8"},
		{name: "public ipv6", in: "2606:4700:4700::1111", want: "2606:4700:4700::1111"},
		{name: "private ipv4", in: "172.31.15.115", want: ""},
		{name: "loopback", in: "127.0.0.1", want: ""},
		{name: "reserved", in: "192.0.2.1", want: ""},
		{name: "garbage", in: "not an ip", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Normalize(tt.in); got != tt.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
