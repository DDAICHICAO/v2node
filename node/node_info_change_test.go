package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
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

func selfTrojanSNIChangeNode(domain string) *panel.NodeInfo {
	info := managedTLSDomainSwitchNode(domain)
	info.Common.ServerName = domain
	info.Common.TlsSettings.CertMode = "self"
	info.Common.TlsSettings.CertificateScopeID = 0
	info.Common.TlsSettings.RejectUnknownSni = "0"
	info.Common.CertInfo.CertMode = "self"
	return info
}

func selfTrojanSNIChangeController(t *testing.T) (*Controller, chan struct{}) {
	t.Helper()
	current := selfTrojanSNIChangeNode("old.example.com")
	reloadCh := make(chan struct{}, 1)
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: current.Id}
	return &Controller{
		apiClient:             &panel.Client{},
		conf:                  &cfg,
		store:                 newOfflineStateStore(t.TempDir()),
		info:                  current,
		tag:                   current.Tag,
		server:                &core.V2Core{ReloadCh: reloadCh},
		userList:              []panel.UserInfo{{Id: 1, Uuid: "test-user"}},
		aliveMap:              map[int]int{1: 1},
		deviceAliveMap:        map[int]int{1: 1},
		activeNodeInfoVersion: "old-version",
	}, reloadCh
}

func TestSelfTrojanSNIChangeDoesNotReload(t *testing.T) {
	for _, domain := range []string{"new.example.com", "", "192.0.2.1"} {
		t.Run("sni="+domain, func(t *testing.T) {
			c, reloadCh := selfTrojanSNIChangeController(t)
			server := c.server
			users := append([]panel.UserInfo(nil), c.userList...)
			c.pendingNodeInfo = selfTrojanSNIChangeNode(domain)
			c.pendingNodeInfoVersion = "new-version"
			if err := c.applyPendingNodeInfo(); err != nil {
				t.Fatal(err)
			}
			if len(reloadCh) != 0 {
				t.Fatal("SNI-only change queued a global reload")
			}
			if c.server != server || !reflect.DeepEqual(c.userList, users) {
				t.Fatal("SNI-only change replaced the core or changed users")
			}
			if c.info.Common.TlsSettings.ServerName != domain || c.info.Common.CertInfo.CertDomain != domain {
				t.Fatal("active TLS metadata was not updated")
			}
			if c.activeNodeInfoVersion != "new-version" || c.pendingNodeInfo != nil || c.pendingNodeInfoVersion != "" {
				t.Fatal("applied config version or pending state is stale")
			}
			state, err := c.store.Load(*c.conf)
			if err != nil {
				t.Fatal(err)
			}
			// Compare the persisted representation: nil RawMessage fields round-trip
			// through JSON as the literal null rather than retaining a nil slice.
			savedJSON, err := json.Marshal(offlineState{NodeInfo: state.NodeInfo, Users: state.Users})
			if err != nil {
				t.Fatal(err)
			}
			activeJSON, err := json.Marshal(offlineState{NodeInfo: c.info, Users: users})
			if err != nil {
				t.Fatal(err)
			}
			if string(savedJSON) != string(activeJSON) {
				t.Fatal("offline snapshot does not match the active config and users")
			}
			c.pendingNodeInfo = selfTrojanSNIChangeNode(domain)
			c.pendingNodeInfoVersion = "new-version"
			if err := c.applyPendingNodeInfo(); err != nil || len(reloadCh) != 0 {
				t.Fatal("observing the same SNI again triggered a reload")
			}
			c.pendingNodeInfo = selfTrojanSNIChangeNode(domain)
			c.pendingNodeInfo.Common.ServerPort++
			if err := c.applyPendingNodeInfo(); err != nil || len(reloadCh) != 1 {
				t.Fatal("a later port change did not retain the reload path")
			}
		})
	}
}

func TestSelfTrojanSNIChangeKeepsPendingOnPersistenceFailure(t *testing.T) {
	c, reloadCh := selfTrojanSNIChangeController(t)
	notDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	c.store = newOfflineStateStore(notDirectory)
	c.pendingNodeInfo = selfTrojanSNIChangeNode("new.example.com")
	c.pendingNodeInfoVersion = "new-version"
	if err := c.applyPendingNodeInfo(); err == nil {
		t.Fatal("expected snapshot persistence failure")
	}
	if len(reloadCh) != 0 || c.info.Common.TlsSettings.ServerName != "old.example.com" || c.activeNodeInfoVersion != "old-version" {
		t.Fatal("persistence failure reloaded or changed the active state")
	}
	if c.pendingNodeInfo == nil || c.pendingNodeInfoVersion != "new-version" {
		t.Fatal("failed SNI change was not retained for retry")
	}
	c.store = newOfflineStateStore(t.TempDir())
	if err := c.applyPendingNodeInfo(); err != nil {
		t.Fatal(err)
	}
	if len(reloadCh) != 0 || c.info.Common.TlsSettings.ServerName != "new.example.com" || c.pendingNodeInfo != nil {
		t.Fatal("retry did not apply the SNI change without reload")
	}
	state, err := c.store.Load(*c.conf)
	if err != nil || state.NodeInfo.Common.TlsSettings.ServerName != "new.example.com" {
		t.Fatal("retry did not persist the new SNI")
	}
}

func TestSelfTrojanSNIChangeKeepsReloadForUnsafeChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(current, next *panel.NodeInfo)
	}{
		{"port", func(_, n *panel.NodeInfo) { n.Common.ServerPort++ }},
		{"protocol", func(_, n *panel.NodeInfo) { n.Type, n.Common.Protocol = "vless", "vless" }},
		{"node identity", func(_, n *panel.NodeInfo) { n.Id++ }},
		{"certificate path", func(_, n *panel.NodeInfo) { n.Common.CertInfo.CertFile = "other.pem" }},
		{"key path", func(_, n *panel.NodeInfo) { n.Common.CertInfo.KeyFile = "other.key" }},
		{"certificate mode", func(_, n *panel.NodeInfo) { n.Common.CertInfo.CertMode = "file" }},
		{"TLS mode", func(_, n *panel.NodeInfo) { n.Common.TlsSettings.CertMode = "file" }},
		{"reject unknown SNI enabled", func(_, n *panel.NodeInfo) { n.Common.CertInfo.RejectUnknownSni = true }},
		{"reject unknown SNI retained", func(c, n *panel.NodeInfo) {
			c.Common.CertInfo.RejectUnknownSni, n.Common.CertInfo.RejectUnknownSni = true, true
			c.Common.TlsSettings.RejectUnknownSni, n.Common.TlsSettings.RejectUnknownSni = "1", "1"
		}},
		{"reject flag mismatch", func(c, n *panel.NodeInfo) {
			c.Common.TlsSettings.RejectUnknownSni, n.Common.TlsSettings.RejectUnknownSni = "1", "1"
		}},
		{"file mode", func(c, n *panel.NodeInfo) {
			c.Common.CertInfo.CertMode, n.Common.CertInfo.CertMode = "file", "file"
			c.Common.TlsSettings.CertMode, n.Common.TlsSettings.CertMode = "file", "file"
		}},
		{"different protocol retained", func(c, n *panel.NodeInfo) {
			c.Type, n.Type, c.Common.Protocol, n.Common.Protocol = "tuic", "tuic", "tuic", "tuic"
		}},
		{"certificate scope", func(c, n *panel.NodeInfo) {
			c.Common.TlsSettings.CertificateScopeID, n.Common.TlsSettings.CertificateScopeID = 1, 1
		}},
		{"missing certificate path", func(c, n *panel.NodeInfo) { c.Common.CertInfo.CertFile, n.Common.CertInfo.CertFile = "", "" }},
		{"missing key path", func(c, n *panel.NodeInfo) { c.Common.CertInfo.KeyFile, n.Common.CertInfo.KeyFile = "", "" }},
		{"missing cert info", func(c, n *panel.NodeInfo) { c.Common.CertInfo, n.Common.CertInfo = nil, nil }},
		{"routes also changed", func(_, n *panel.NodeInfo) { n.Common.Routes = []panel.Route{{Id: 9, Action: "block"}} }},
		{"transport also changed", func(_, n *panel.NodeInfo) { n.Common.Network = "ws" }},
		{"security also changed", func(_, n *panel.NodeInfo) { n.Security = panel.Reality }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current, next := selfTrojanSNIChangeNode("old.example.com"), selfTrojanSNIChangeNode("new.example.com")
			tc.mutate(current, next)
			if got := classifyNodeInfoChange(current, next); got != nodeInfoFullReload {
				t.Fatalf("unsafe change kind=%v, want full reload", got)
			}
		})
	}
}
