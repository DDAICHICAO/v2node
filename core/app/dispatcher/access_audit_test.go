package dispatcher

import "testing"

func TestExtractNodeIDFromInboundTag(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want uint32
	}{
		{name: "standard tag", tag: "[https://panel.example.com]-shadowsocks:514", want: 514},
		{name: "url contains port", tag: "[https://panel.example.com:8443]-vless:9", want: 9},
		{name: "empty tag", tag: "", want: 0},
		{name: "malformed tag", tag: "sntp-eclipse", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractNodeIDFromInboundTag(tt.tag); got != tt.want {
				t.Fatalf("expected %d, got %d", tt.want, got)
			}
		})
	}
}
