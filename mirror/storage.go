package mirror

import (
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/cybozu-go/aptutil/apt"
	"github.com/pkg/errors"
)

const (
	infoJSON = "info.json"
)

// Storage manages a directory tree that mirrors a Debian repository.
//
// Storage also keeps checksum information for stored files.
type Storage struct {
	dir    string
	prefix string
	syncer *fsSyncer

	mu   sync.RWMutex
	info map[string]*apt.FileInfo

	// dirs is the set of directories known to exist.
	dirs sync.Map
}

// NewStorage constructs Storage.
//
// dir must be an absolute path to an existing directory.
// prefix should be a directory name.
//
// NewStorage must be called before files are stored so that Save can
// detect errors in writing them back.  The returned Storage should be
// closed by Close.
func NewStorage(dir, prefix string) (*Storage, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("none absolute: " + dir)
	}

	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsDir() {
		return nil, errors.New("not a directory: " + dir)
	}

	syncer, err := newFSSyncer(dir)
	if err != nil {
		return nil, err
	}

	return &Storage{
		dir:    dir,
		prefix: prefix,
		syncer: syncer,
		info:   make(map[string]*apt.FileInfo),
	}, nil
}

// Close releases resources of the Storage.
func (s *Storage) Close() error {
	return s.syncer.Close()
}

// Dir returns the directory of the Storage.
func (s *Storage) Dir() string {
	return s.dir
}

// Load loads existing directory contents.
func (s *Storage) Load() error {
	infoPath := filepath.Join(s.dir, infoJSON)

	f, err := os.Open(infoPath) //nolint:gosec // G304: info file lives in the storage directory
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return err
	}
	defer func() { _ = f.Close() }()

	jd := json.NewDecoder(f)
	err = jd.Decode(&s.info)
	if err != nil {
		return errors.Wrap(err, "Storage.Load: "+infoPath)
	}
	return nil
}

// TempFile creates a new temporary file
// in the directory specified in Storage,
// opens the file for reading and writing,
// and returns the resulting *os.File.
func (s *Storage) TempFile() (*os.File, error) {
	return os.CreateTemp(s.dir, "_tmp")
}

// Save saves storage contents persistently.
func (s *Storage) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	infoPath := filepath.Join(s.dir, infoJSON)
	f, err := os.OpenFile(infoPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // G304: info file lives in the storage directory
	if err != nil {
		return err
	}

	enc := json.NewEncoder(f)
	err = enc.Encode(s.info)
	if err != nil {
		_ = f.Close()
		return err
	}

	err = f.Close()
	if err != nil {
		return err
	}

	// files are not synced one by one when they are stored, so sync
	// them all at once together with info.json and directories.
	err = s.syncer.Sync()
	if err != nil {
		return errors.Wrap(err, "syncer.Sync")
	}

	return nil
}

// mkdirAll is os.MkdirAll that remembers the directories created, so
// that storing many files in a directory does not check it every time.
func (s *Storage) mkdirAll(d string) error {
	if _, ok := s.dirs.Load(d); ok {
		return nil
	}
	err := os.MkdirAll(d, 0o755)
	if err != nil {
		return err
	}
	s.dirs.Store(d, struct{}{})
	return nil
}

// StoreLink stores a hard link to a file into this storage.
func (s *Storage) StoreLink(fi *apt.FileInfo, fullpath string) error {
	p := fi.Path()

	s.mu.Lock()
	_, ok := s.info[p]
	if ok {
		s.mu.Unlock()
		return errors.New("already stored: " + p)
	}
	s.info[p] = fi
	s.mu.Unlock()

	fp := filepath.Join(s.dir, s.prefix, filepath.Clean(p))
	d := filepath.Dir(fp)

	err := s.mkdirAll(d)
	if err != nil {
		return err
	}

	return os.Link(fullpath, fp)
}

// byHashPaths returns the by-hash paths of fi, strongest first.
// Paths for checksums that fi does not have are omitted.
func byHashPaths(fi *apt.FileInfo) []string {
	var l []string
	for _, p := range []string{fi.SHA512Path(), fi.SHA256Path(), fi.SHA1Path(), fi.MD5SumPath()} {
		if p != "" {
			l = append(l, p)
		}
	}
	return l
}

// byHashPath returns the strongest by-hash path of fi,
// or an empty string if fi has no checksum.
func byHashPath(fi *apt.FileInfo) string {
	l := byHashPaths(fi)
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

// StoreLinkWithHash stores a hard link to a file into this storage
// with additional hard links for by-hash retrieval.
func (s *Storage) StoreLinkWithHash(fi *apt.FileInfo, fullpath string) error {
	p := fi.Path()
	hashPaths := byHashPaths(fi)

	var fpl []string
	s.mu.Lock()
	if _, ok := s.info[p]; !ok {
		// otherwise ignore the canonical path because another file
		// was already stored.
		s.info[p] = fi
		fpl = append(fpl, filepath.Join(s.dir, s.prefix, filepath.Clean(p)))
	}

	// This may overwrite existing entries in s.info if another item
	// accidentally has the same checksums.  In such cases, Storage.Lookup
	// for the previous item will return nil and go-apt-mirror would
	// fail to reuse the item.
	//
	// Although we may fix the problem in Storage.Lookup, at this point
	// we leave it as it is not too bad.
	for _, hp := range hashPaths {
		s.info[hp] = fi
		fpl = append(fpl, filepath.Join(s.dir, s.prefix, filepath.Clean(hp)))
	}
	s.mu.Unlock()

	for _, fp := range fpl {
		d := filepath.Dir(fp)
		err := s.mkdirAll(d)
		if err != nil {
			return errors.Wrap(err, "StoreLinkWithHash: "+fp)
		}
		err = os.Link(fullpath, fp)
		if err != nil && !os.IsExist(err) {
			return errors.Wrap(err, "StoreLinkWithHash: "+fp)
		}
	}
	return nil
}

// Lookup looks up a file in this storage.
//
// If a file matching fi exists, its info and full path is returned.
// Otherwise, nil and empty string is returned.
//
// A file recorded in info.json is not found if it is missing or its
// size differs on disk, e.g. removed by hand, so that it is downloaded
// again instead of failing to be reused forever.
func (s *Storage) Lookup(fi *apt.FileInfo, byhash bool) (*apt.FileInfo, string) {
	f := func(p string) (*apt.FileInfo, string) {
		s.mu.RLock()
		fi2, ok := s.info[p]
		s.mu.RUnlock()
		if !ok {
			return nil, ""
		}
		// info.json written by older versions has no SHA512, so
		// calculate it to reuse the file instead of downloading it.
		lacksSHA512 := fi.SHA512Path() != "" && fi2.SHA512Path() == ""
		if !lacksSHA512 && !fi.Same(fi2) {
			return nil, ""
		}

		fullpath := filepath.Join(s.dir, s.prefix, filepath.Clean(p))
		st, err := os.Lstat(fullpath)
		if err != nil || !st.Mode().IsRegular() || uint64(st.Size()) != fi2.Size() { //nolint:gosec // G115: regular file sizes are non-negative
			return nil, ""
		}
		if lacksSHA512 {
			fi2, err = calcFileInfo(fullpath, fi2.Path())
			if err != nil || !fi.Same(fi2) {
				return nil, ""
			}
		}
		return fi2, fullpath
	}

	if hp := byHashPath(fi); byhash && hp != "" {
		fi2, fullpath := f(hp)
		if fi2 != nil {
			return fi2, fullpath
		}
	}

	return f(fi.Path())
}

// isByHashPath returns true if p is a by-hash path such as
// "dists/s/main/binary-amd64/by-hash/SHA256/<checksum>".
func isByHashPath(p string) bool {
	return path.Base(path.Dir(path.Dir(p))) == "by-hash"
}

// KeepByHash stores hard links to the by-hash files of the indices in
// prev, the storage of the previous mirror, unless s has them.
//
// A client that has retrieved Release from the previous mirror retrieves
// the indices listed in it by hash after the mirror is replaced.  Only
// the by-hash files of the indices at their canonical paths in prev are
// kept, so that by-hash files are kept for one generation.
func (s *Storage) KeepByHash(prev *Storage) error {
	prev.mu.RLock()
	defer prev.mu.RUnlock()

	for p, fi := range prev.info {
		if isByHashPath(p) {
			continue
		}
		for _, hp := range byHashPaths(fi) {
			prevfi, ok := prev.info[hp]
			if !ok || !prevfi.Same(fi) {
				continue
			}

			s.mu.Lock()
			_, ok = s.info[hp]
			if !ok {
				s.info[hp] = prevfi
			}
			s.mu.Unlock()
			if ok {
				continue
			}

			fp := filepath.Join(s.dir, s.prefix, filepath.Clean(hp))
			if err := s.mkdirAll(filepath.Dir(fp)); err != nil {
				return errors.Wrap(err, "KeepByHash: "+hp)
			}
			err := os.Link(filepath.Join(prev.dir, prev.prefix, filepath.Clean(hp)), fp)
			if os.IsNotExist(err) {
				// removed by hand.
				s.mu.Lock()
				delete(s.info, hp)
				s.mu.Unlock()
				continue
			}
			if err != nil {
				return errors.Wrap(err, "KeepByHash: "+hp)
			}
		}
	}
	return nil
}

// calcFileInfo calculates FileInfo of the file at fullpath stored at p.
func calcFileInfo(fullpath, p string) (*apt.FileInfo, error) {
	f, err := os.Open(fullpath) //nolint:gosec // G304: fullpath is a file in the mirror directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return apt.CopyWithFileInfo(io.Discard, f, p)
}

// Open opens the named file and returns it.
func (s *Storage) Open(p string) (*os.File, error) {
	return os.Open(filepath.Join(s.dir, s.prefix, filepath.Clean(p)))
}
