package core

import (
	"fmt"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func (v *V2Core) AddNode(tag string, info *panel.NodeInfo) error {
	if isSntpEclipseNode(info) {
		if v.eclipse == nil {
			v.eclipse = make(map[string]*SntpEclipseServer)
		}
		server, err := newSntpEclipseServer(tag, info)
		if err != nil {
			return err
		}
		if err := server.Start(); err != nil {
			return err
		}
		v.eclipse[tag] = server
		return nil
	}
	inBoundConfig, err := buildInbound(info, tag)
	if err != nil {
		return fmt.Errorf("build inbound error: %s", err)
	}
	err = v.addInbound(inBoundConfig)
	if err != nil {
		return fmt.Errorf("add inbound error: %s", err)
	}
	return nil
}

func (v *V2Core) DelNode(tag string) error {
	if server, ok := v.eclipse[tag]; ok {
		delete(v.eclipse, tag)
		return server.Close()
	}
	err := v.removeInbound(tag)
	if err != nil {
		return fmt.Errorf("remove in error: %s", err)
	}
	return nil
}
