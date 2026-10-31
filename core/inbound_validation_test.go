package core

import (
	"strings"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestBuildInboundRejectsInvalidSNIFrontModeBeforeCreatingConfig(t *testing.T) {
	info := &panel.NodeInfo{
		Type:     "vless",
		Security: panel.Tls,
		Tag:      "test-vless",
		Common: &panel.CommonNode{
			Protocol:   "vless",
			ListenIP:   "0.0.0.0",
			Network:    "tcp",
			ServerPort: 10011,
			Tls:        panel.Tls,
			TlsSettings: panel.TlsSettings{
				FrontMode:  "sni",
				ServerName: "192.0.2.10",
				CertMode:   "file",
			},
			CertInfo: &panel.CertInfo{
				CertMode: "none",
			},
		},
	}

	_, err := buildInbound(info, info.Tag)
	if err == nil || !strings.Contains(err.Error(), "valid DNS SNI") {
		t.Fatalf("error=%v, want SNI validation failure", err)
	}
}
