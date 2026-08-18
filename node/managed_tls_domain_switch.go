package node

import (
	"reflect"
	"strings"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func managedTLSDomainOnlyChange(current, next *panel.NodeInfo) (string, bool) {
	if current == nil || next == nil || current.Common == nil || next.Common == nil ||
		current.Common.CertInfo == nil || next.Common.CertInfo == nil ||
		current.Security != panel.Tls || next.Security != panel.Tls ||
		!strings.EqualFold(current.Common.TlsSettings.CertMode, "managed") ||
		!strings.EqualFold(next.Common.TlsSettings.CertMode, "managed") ||
		!strings.EqualFold(current.Common.CertInfo.CertMode, "managed") ||
		!strings.EqualFold(next.Common.CertInfo.CertMode, "managed") ||
		current.Common.TlsSettings.CertificateScopeID == 0 ||
		next.Common.TlsSettings.CertificateScopeID == 0 {
		return "", false
	}

	currentDomain := current.Common.TlsSettings.PrimaryServerName()
	target := strings.TrimSuffix(
		strings.ToLower(strings.TrimSpace(next.Common.TlsSettings.PrimaryServerName())),
		".",
	)
	if !validManagedTLSDomain(currentDomain) || !validManagedTLSDomain(target) ||
		sameManagedTLSDomain(currentDomain, target) {
		return "", false
	}

	left := cloneNodeInfoForManagedTLSDomainComparison(current)
	right := cloneNodeInfoForManagedTLSDomainComparison(next)
	if !reflect.DeepEqual(left, right) {
		return "", false
	}
	return target, true
}

func cloneNodeInfoForManagedTLSDomainComparison(info *panel.NodeInfo) *panel.NodeInfo {
	clone := *info
	common := *info.Common
	clone.Common = &common
	common.TlsSettings.ServerName = ""
	common.TlsSettings.ServerNames = nil
	if info.Common.CertInfo != nil {
		certInfo := *info.Common.CertInfo
		certInfo.CertDomain = ""
		common.CertInfo = &certInfo
	}
	return &clone
}
