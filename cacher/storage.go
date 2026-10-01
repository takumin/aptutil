package cacher

import (
	"container/heap"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/cybozu-go/aptutil/apt"
	"github.com/cybozu-go/log"
	"github.com/pkg/errors"
)

const (
	fileSuffix = ".cache"
)

var (
	// ErrNotFound is returned by Storage.Lookup for non-existing items.
	ErrNotFound = errors.New("not found")

	// ErrBadPath is returned by Storage.Insert if path is bad
	ErrBadPath = errors.New("bad path")
)

// entry represents an item in the cache.
type entry struct {
	*apt.FileInfo

	// for container/heap.
	// atime is used as priorities.
	atime uint64
	index int
}

// FilePath returns the filename of the entry.
func (e *entry) FilePath() string {
	return e.Path() + fileSuffix
}

// Storage stores cache items in local file system.
//
// Cached items will be removed in LRU fashion when the total size of
// items exceeds the capacity.
type Storage struct {
	dir      string // directory for cache items
	capacity uint64

	mu     sync.Mutex
	used   uint64
	cache  map[string]*entry
	lru    []*entry // for container/heap
	lclock uint64   // ditto
}

// NewStorage creates a Storage.
//
// dir is the directory for cached items.
// capacity is the maximum total size (bytes) of items in the cache.
// If capacity is zero, items will not be evicted.
// Non-existing directories will be created (insufficient permission result in panic)
func NewStorage(dir string, capacity uint64) *Storage {
	if !filepath.IsAbs(dir) {
		panic("dir must be an absolute path")
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		err = os.MkdirAll(dir, 0o755)
		if err != nil {
			panic("Storage.NewStorage: failed to create " + dir)
		}
	}

	return &Storage{
		dir:      dir,
		cache:    make(map[string]*entry),
		capacity: capacity,
	}
}

// Len implements heap.Interface.
func (cm *Storage) Len() int {
	return len(cm.lru)
}

// Less implements heap.Interface.
func (cm *Storage) Less(i, j int) bool {
	return cm.lru[i].atime < cm.lru[j].atime
}

// Swap implements heap.Interface.
func (cm *Storage) Swap(i, j int) {
	cm.lru[i], cm.lru[j] = cm.lru[j], cm.lru[i]
	cm.lru[i].index = i
	cm.lru[j].index = j
}

// Push implements heap.Interface.
func (cm *Storage) Push(x interface{}) {
	e, ok := x.(*entry)
	if !ok {
		panic("Storage.Push: wrong type")
	}
	n := len(cm.lru)
	e.index = n
	cm.lru = append(cm.lru, e)
}

// Pop implements heap.Interface.
func (cm *Storage) Pop() interface{} {
	n := len(cm.lru)
	e := cm.lru[n-1]
	e.index = -1 // for safety
	cm.lru = cm.lru[0 : n-1]
	return e
}

// maint removes unused items from cache until used < capacity.
// cm.mu lock must be acquired beforehand.
func (cm *Storage) maint() {
	for cm.capacity > 0 && cm.used > cm.capacity {
		e := heap.Pop(cm).(*entry)
		delete(cm.cache, e.Path())
		cm.used -= e.Size()
		if err := os.Remove(filepath.Join(cm.dir, e.FilePath())); err != nil {
			log.Warn("Storage.maint", map[string]interface{}{
				"error": err.Error(),
			})
		}
		log.Info("removed", map[string]interface{}{
			"path": e.Path(),
		})
	}
}

// Load loads existing items in filesystem.
func (cm *Storage) Load() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	wf := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		subpath, err := filepath.Rel(cm.dir, path)
		if err != nil {
			return err
		}
		if filepath.Ext(subpath) != fileSuffix {
			return nil
		}
		subpath = subpath[:len(subpath)-len(fileSuffix)]
		if _, ok := cm.cache[subpath]; ok {
			return nil
		}

		size := uint64(info.Size()) //nolint:gosec // G115: regular file sizes are non-negative
		e := &entry{
			// delay calculation of checksums.
			FileInfo: apt.MakeFileInfoNoChecksum(subpath, size),
			atime:    cm.lclock,
			index:    len(cm.lru),
		}
		cm.used += size
		cm.lclock++
		cm.lru = append(cm.lru, e)
		cm.cache[subpath] = e
		log.Debug("Storage.Load", map[string]interface{}{
			"path": subpath,
		})
		return nil
	}

	if err := filepath.Walk(cm.dir, wf); err != nil {
		return err
	}
	heap.Init(cm)

	cm.maint()

	return nil
}

// TempFile creates a new temporary file
// in the directory specified in Storage,
// opens the file for reading and writing,
// and returns the resulting *os.File.
func (cm *Storage) TempFile() (*os.File, error) {
	return os.CreateTemp(cm.dir, "_tmp")
}

// Insert inserts or updates a cache item.
//
// fi.Path() must be as clean as filepath.Clean() and
// must not be filepath.IsAbs().
func (cm *Storage) Insert(filename string, fi *apt.FileInfo) error {
	p := fi.Path()
	switch {
	case p != filepath.Clean(p):
		return ErrBadPath
	case !apt.IsSafePath(p):
		// reject paths that escape the cache root via "..".
		return ErrBadPath
	}

	destpath := filepath.Join(cm.dir, p+fileSuffix)
	dirpath := filepath.Dir(destpath)

	_, err := os.Stat(dirpath) //nolint:gosec // G703: p is validated to stay within cm.dir
	switch {
	case os.IsNotExist(err):
		err = os.MkdirAll(dirpath, 0o755) //nolint:gosec // G703: p is validated to stay within cm.dir
		if err != nil {
			return err
		}
	case err != nil:
		return err
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	if existing, ok := cm.cache[p]; ok {
		err = os.Remove(destpath) //nolint:gosec // G703: p is validated to stay within cm.dir
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			log.Warn("cache file was removed already", map[string]interface{}{
				"path": p,
			})
		}
		cm.used -= existing.Size()
		heap.Remove(cm, existing.index)
		delete(cm.cache, p)
		if log.Enabled(log.LvDebug) {
			log.Debug("deleted existing item", map[string]interface{}{
				"path": p,
			})
		}
	}

	err = os.Link(filename, destpath)
	if err != nil {
		return err
	}

	e := &entry{
		FileInfo: fi,
		atime:    cm.lclock,
	}
	cm.used += fi.Size()
	cm.lclock++
	heap.Push(cm, e)
	cm.cache[p] = e

	cm.maint()

	return nil
}

// calcChecksum streams the file at filename to calculate checksums of
// the item at p.
func calcChecksum(filename, p string) (*apt.FileInfo, error) {
	f, err := os.Open(filename) //nolint:gosec // G304: filename is built from the cache directory and a validated item path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return apt.CopyWithFileInfo(io.Discard, f, p)
}

// Lookup looks up an item in the cache.
// If no item matching fi is found, ErrNotFound is returned.
//
// The caller is responsible to close the returned os.File.
func (cm *Storage) Lookup(fi *apt.FileInfo) (*os.File, error) {
	p := fi.Path()

	cm.mu.Lock()
	defer cm.mu.Unlock()

	for {
		e, ok := cm.cache[p]
		if !ok {
			return nil, ErrNotFound
		}
		if e.HasChecksum() {
			return cm.open(fi, e)
		}

		// Items loaded by Load have no checksums yet.  Calculate them
		// without holding cm.mu as it takes a while for large files.
		filename := filepath.Join(cm.dir, e.FilePath())
		cm.mu.Unlock()
		fi2, err := calcChecksum(filename, p)
		cm.mu.Lock()
		if err != nil {
			return nil, err
		}

		// The item may have been replaced or removed meanwhile.
		// If so, look it up again.
		if cm.cache[p] == e && !e.HasChecksum() {
			cm.used = cm.used - e.Size() + fi2.Size()
			e.FileInfo = fi2
		}
	}
}

// open opens the file of e if it matches fi.
// cm.mu lock must be acquired beforehand.
func (cm *Storage) open(fi *apt.FileInfo, e *entry) (*os.File, error) {
	if !fi.Same(e.FileInfo) {
		// checksum mismatch
		return nil, ErrNotFound
	}

	e.atime = cm.lclock
	cm.lclock++
	heap.Fix(cm, e.index)
	return os.Open(filepath.Join(cm.dir, e.FilePath()))
}

// ListAll returns a list of *apt.FileInfo for all cached items.
func (cm *Storage) ListAll() []*apt.FileInfo {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	l := make([]*apt.FileInfo, cm.Len())
	for i, e := range cm.lru {
		l[i] = e.FileInfo
	}
	return l
}

// Delete deletes an item from the cache.
func (cm *Storage) Delete(p string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	e, ok := cm.cache[p]
	if !ok {
		return nil
	}

	err := os.Remove(filepath.Join(cm.dir, e.FilePath()))
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		log.Warn("cached file was already removed", map[string]interface{}{
			"path": p,
		})
	}

	cm.used -= e.Size()
	heap.Remove(cm, e.index)
	delete(cm.cache, p)
	log.Info("deleted item", map[string]interface{}{
		"path": p,
	})
	return nil
}
