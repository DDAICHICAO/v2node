package node

import (
	"reflect"

	panel "github.com/wyx2685/v2node/api/v2board"
)

type nodeInfoChangeKind uint8

const (
	nodeInfoUnchanged nodeInfoChangeKind = iota
	nodeInfoManagedTLSDomainOnly
	nodeInfoSelfTrojanSNIOnly
	nodeInfoRoutesOnly
	nodeInfoFullReload
)

func classifyNodeInfoChange(current, next *panel.NodeInfo) nodeInfoChangeKind {
	if reflect.DeepEqual(current, next) {
		return nodeInfoUnchanged
	}
	if _, ok := managedTLSDomainOnlyChange(current, next); ok {
		return nodeInfoManagedTLSDomainOnly
	}
	if selfTrojanSNIOnlyChange(current, next) {
		return nodeInfoSelfTrojanSNIOnly
	}
	if current == nil || next == nil || current.Common == nil || next.Common == nil {
		return nodeInfoFullReload
	}

	currentCopy, nextCopy := *current, *next
	currentCommon, nextCommon := *current.Common, *next.Common
	currentCommon.Routes = nil
	nextCommon.Routes = nil
	currentCopy.Common = &currentCommon
	nextCopy.Common = &nextCommon
	if reflect.DeepEqual(&currentCopy, &nextCopy) {
		return nodeInfoRoutesOnly
	}
	return nodeInfoFullReload
}

func selfTrojanSNIOnlyChange(current, next *panel.NodeInfo) bool {
	for _, info := range []*panel.NodeInfo{current, next} {
		if info == nil || info.Common == nil || info.Common.CertInfo == nil {
			return false
		}
		tls, cert := info.Common.TlsSettings, info.Common.CertInfo
		if info.Type != "trojan" || info.Common.Protocol != "trojan" || info.Security != panel.Tls ||
			tls.CertMode != "self" || cert.CertMode != "self" || tls.CertificateScopeID != 0 ||
			cert.RejectUnknownSni || (tls.RejectUnknownSni != "" && tls.RejectUnknownSni != "0") ||
			cert.CertFile == "" || cert.KeyFile == "" {
			return false
		}
	}

	// Self certificates are reused, and permissive Trojan TLS does not consume
	// these names when constructing its inbound. Keep every other field in the
	// comparison so certificate, listener, route and policy changes still reload.
	left, right := *current, *next
	leftCommon, rightCommon := *current.Common, *next.Common
	leftCert, rightCert := *current.Common.CertInfo, *next.Common.CertInfo
	for _, common := range []*panel.CommonNode{&leftCommon, &rightCommon} {
		common.ServerName = ""
		common.TlsSettings.ServerName = ""
		common.TlsSettings.ServerNames = nil
	}
	leftCert.CertDomain, rightCert.CertDomain = "", ""
	leftCommon.CertInfo, rightCommon.CertInfo = &leftCert, &rightCert
	left.Common, right.Common = &leftCommon, &rightCommon
	return reflect.DeepEqual(&left, &right)
}
