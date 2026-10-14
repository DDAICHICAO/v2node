package core

import (
	"fmt"
	"os"

	panel "github.com/wyx2685/v2node/api/v2board"
)

// ValidateNode constructs protocol configuration without opening a listener.
func (v *V2Core) ValidateNode(info *panel.NodeInfo, users []panel.UserInfo) error {
	if info == nil || info.Common == nil || info.Common.BaseConfig == nil || info.Tag == "" {
		return fmt.Errorf("incomplete node configuration")
	}
	if info.Common.ServerPort < 1 || info.Common.ServerPort > 65535 {
		return fmt.Errorf("invalid node port")
	}
	if isSntpEclipseNode(info) {
		candidate, err := newSntpEclipseServer(info.Tag, info, v.Server, v.dispatcher)
		if err != nil {
			return err
		}
		return candidate.Close()
	}
	if isMieruNode(info) {
		candidate, err := newMieruServer(info.Tag, info, v.Server, v.dispatcher)
		if err != nil {
			return err
		}
		return candidate.Close()
	}
	// Managed issuance is deliberately asynchronous and can wait without a listener.
	if info.Common.TlsSettings.CertMode == "managed" && info.Common.CertInfo != nil {
		if _, err := os.Stat(info.Common.CertInfo.CertFile); os.IsNotExist(err) {
			return nil
		}
	}
	if _, err := buildInbound(info, info.Tag); err != nil {
		return err
	}
	if info.Type == "shadowsocks" && info.Common.ServerKey != "" {
		keyLength := 0
		switch info.Common.Cipher {
		case "2022-blake3-aes-128-gcm":
			keyLength = 16
		case "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
			keyLength = 32
		}
		for _, user := range users {
			if len(user.Uuid) < keyLength {
				return fmt.Errorf("shadowsocks credential is too short")
			}
		}
	}
	converted, err := buildCoreUsers(&AddUsersParams{Tag: info.Tag, NodeInfo: info, Users: users})
	if err != nil {
		return err
	}
	for _, user := range converted {
		if _, err := user.ToMemoryUser(); err != nil {
			return err
		}
	}
	return nil
}
