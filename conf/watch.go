package conf

import (
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

func (p *Conf) Watch(filePath string, reload func()) error {
	target, err := filepath.Abs(filePath)
	if err != nil {
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("new watcher error: %w", err)
	}
	if err := watcher.Add(filepath.Dir(target)); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("watch config directory: %w", err)
	}
	go func() {
		defer watcher.Close()
		var timer *time.Timer
		var timerC <-chan time.Time
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		for {
			select {
			case e, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Clean(e.Name) != target || !e.Has(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) {
					continue
				}
				if timer == nil {
					timer = time.NewTimer(250 * time.Millisecond)
				} else {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(250 * time.Millisecond)
				}
				timerC = timer.C
			case <-timerC:
				timerC = nil
				// Validate a separate value. The active core still owns p until the swap.
				candidate := New()
				if err := candidate.LoadFromPath(target); err != nil {
					log.Printf("reload config error, keeping active runtime: %s", err)
					continue
				}
				log.Println("config file changed, preparing reload...")
				reload()
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				if err != nil {
					log.Printf("File watcher error: %s", err)
				}
			}
		}
	}()
	return nil
}
