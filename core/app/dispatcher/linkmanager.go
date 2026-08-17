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

func newLinkManager() *LinkManager {
	return &LinkManager{links: make(map[*ManagedWriter]buf.Reader)}
}

func (m *LinkManager) AddLink(writer *ManagedWriter, reader buf.Reader) {
	m.mu.Lock()
	m.links[writer] = reader
	m.mu.Unlock()
}

func (m *LinkManager) RemoveWriter(writer *ManagedWriter) {
	m.mu.Lock()
	delete(m.links, writer)
	m.mu.Unlock()
}

func (m *LinkManager) CloseAll() int {
	return m.closeMatched(func(*ManagedWriter) bool { return true })
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
		_ = common.Close(w.writer)
		_ = common.Interrupt(readers[i])
	}
	return len(writers)
}

type LinkRegistry struct {
	mu    sync.RWMutex
	users map[string]*LinkManager
}

func NewLinkRegistry() *LinkRegistry {
	return &LinkRegistry{users: make(map[string]*LinkManager)}
}

func (r *LinkRegistry) ActivateUser(user string) {
	if r == nil || user == "" {
		return
	}
	r.mu.Lock()
	if _, exists := r.users[user]; !exists {
		r.users[user] = newLinkManager()
	}
	r.mu.Unlock()
}

func (r *LinkRegistry) RegisterLink(user string, writer buf.Writer, reader buf.Reader, source string) (*ManagedWriter, bool) {
	if r == nil || user == "" {
		_ = common.Close(writer)
		_ = common.Interrupt(reader)
		return nil, false
	}
	r.mu.RLock()
	manager, exists := r.users[user]
	if !exists {
		r.mu.RUnlock()
		_ = common.Close(writer)
		_ = common.Interrupt(reader)
		return nil, false
	}
	managed := &ManagedWriter{writer: writer, manager: manager, source: source}
	manager.AddLink(managed, reader)
	r.mu.RUnlock()
	return managed, true
}

func (r *LinkRegistry) DeactivateUser(user string) int {
	if r == nil || user == "" {
		return 0
	}
	r.mu.Lock()
	manager, exists := r.users[user]
	if exists {
		delete(r.users, user)
	}
	r.mu.Unlock()
	if !exists {
		return 0
	}
	return manager.CloseAll()
}

func (r *LinkRegistry) CloseUserIP(user, ip string) int {
	if r == nil || user == "" || ip == "" {
		return 0
	}
	r.mu.RLock()
	manager, exists := r.users[user]
	r.mu.RUnlock()
	if !exists {
		return 0
	}
	return manager.CloseByIP(ip)
}
