package dispatcher

import (
	sync "sync"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

type ManagedWriter struct {
	writer  buf.Writer
	manager *LinkManager
	source  string
}

func (w *ManagedWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	return w.writer.WriteMultiBuffer(mb)
}

func (w *ManagedWriter) Close() error {
	w.manager.RemoveWriter(w)
	return common.Close(w.writer)
}

type LinkManager struct {
	links map[*ManagedWriter]buf.Reader
	mu    sync.RWMutex
}

func (m *LinkManager) AddLink(writer *ManagedWriter, reader buf.Reader) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.links[writer] = reader
}

func (m *LinkManager) RemoveWriter(writer *ManagedWriter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.links, writer)
}

func (m *LinkManager) CloseAll() {
	m.closeMatched(func(*ManagedWriter) bool { return true })
}

func (m *LinkManager) CloseByIP(ip string) int {
	return m.closeMatched(func(w *ManagedWriter) bool {
		return w.source == ip
	})
}

func (m *LinkManager) closeMatched(match func(*ManagedWriter) bool) int {
	var writers []*ManagedWriter
	var readers []buf.Reader

	m.mu.Lock()
	for w, r := range m.links {
		if match(w) {
			writers = append(writers, w)
			readers = append(readers, r)
			delete(m.links, w)
		}
	}
	m.mu.Unlock()

	for i, w := range writers {
		common.Close(w.writer)
		common.Interrupt(readers[i])
	}
	return len(writers)
}
