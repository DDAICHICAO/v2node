package node

import (
	"reflect"

	panel "github.com/wyx2685/v2node/api/v2board"
)

type nodeInfoChangeKind uint8

const (
	nodeInfoUnchanged nodeInfoChangeKind = iota
	nodeInfoManagedTLSDomainOnly
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
