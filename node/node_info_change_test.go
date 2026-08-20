package node

import (
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestClassifyNodeInfoChange(t *testing.T) {
	base := routeChangeNodeInfo()
	cases := []struct {
		name   string
		mutate func(*panel.NodeInfo)
		want   nodeInfoChangeKind
	}{
		{name: "unchanged", mutate: func(*panel.NodeInfo) {}, want: nodeInfoUnchanged},
		{name: "routes only", mutate: func(n *panel.NodeInfo) {
			n.Common.Routes = []panel.Route{{Id: 9, Action: "block", Match: []string{"domain:example.com"}}}
		}, want: nodeInfoRoutesOnly},
		{name: "port requires reload", mutate: func(n *panel.NodeInfo) {
			n.Common.ServerPort++
		}, want: nodeInfoFullReload},
		{name: "route and port require reload", mutate: func(n *panel.NodeInfo) {
			n.Common.ServerPort++
			n.Common.Routes = []panel.Route{{Id: 9, Action: "block", Match: []string{"domain:example.com"}}}
		}, want: nodeInfoFullReload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := cloneNodeInfoForTest(base)
			tc.mutate(next)
			if got := classifyNodeInfoChange(base, next); got != tc.want {
				t.Fatalf("kind=%v, want %v", got, tc.want)
			}
		})
	}
}

func routeChangeNodeInfo() *panel.NodeInfo {
	return &panel.NodeInfo{
		Id:           1,
		Type:         "vless",
		Security:     panel.None,
		PushInterval: time.Minute,
		PullInterval: time.Minute,
		Tag:          "node-1",
		Common: &panel.CommonNode{
			Protocol:   "vless",
			ListenIP:   "0.0.0.0",
			ServerPort: 443,
			BaseConfig: &panel.BaseConfig{},
		},
	}
}

func cloneNodeInfoForTest(info *panel.NodeInfo) *panel.NodeInfo {
	clone := *info
	common := *info.Common
	clone.Common = &common
	common.Routes = append([]panel.Route(nil), info.Common.Routes...)
	return &clone
}
