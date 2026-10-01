package cacher

// This file implements core logics to download and cache APT
// repository items.

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cybozu-go/aptutil/apt"
	"github.com/cybozu-go/aptutil/internal/stall"
	"github.com/cybozu-go/log"
	"github.com/cybozu-go/well"
	"github.com/pkg/errors"
)

const (
	gib = 1 << 30

	// stallTimeout is how long a request to an upstream server may make
	// no progress, either waiting for the response header or reading
	// the body, before it is aborted.
	stallTimeout = 1 * time.Minute
)

// addPrefix add prefix for each *FileInfo in fil.
func addPrefix(prefix string, fil []*apt.FileInfo) []*apt.FileInfo {
	ret := make([]*apt.FileInfo, 0, len(fil))
	for _, fi := range fil {
		ret = append(ret, fi.AddPrefix(prefix))
	}
	return ret
}

// byHashAliases returns fil keyed by their by-hash paths.
//
// Meta data files compressed in unsupported formats are omitted as
// they cannot be cached as meta data files.
func byHashAliases(fil []*apt.FileInfo) map[string]*apt.FileInfo {
	m := make(map[string]*apt.FileInfo)
	for _, fi := range fil {
		if apt.IsMeta(fi.Path()) && !apt.IsSupported(fi.Path()) {
			continue
		}
		for _, hp := range []string{fi.SHA256Path(), fi.SHA1Path(), fi.MD5SumPath()} {
			if hp != "" {
				m[hp] = fi
			}
		}
	}
	return m
}

// Cacher downloads and caches APT indices and deb files.
type Cacher struct {
	meta          *Storage
	items         *Storage
	um            URLMap
	checkInterval time.Duration
	cachePeriod   time.Duration
	client        *http.Client
	maxConns      int

	// stallTimeout aborts a request that makes no progress for it.
	stallTimeout time.Duration

	fiLock sync.RWMutex
	// info has FileInfo of meta data files and items listed in them.
	// Items not listed anywhere are looked up by path in Storage.
	info map[string]*apt.FileInfo
	// aliases maps by-hash paths to FileInfo of the files they refer.
	aliases map[string]*apt.FileInfo
	// listed maps a meta data file to the items and by-hash paths it
	// lists, and refs counts how many meta data files list each of them.
	// They are used to remove stale entries from info and aliases when
	// meta data files are updated.
	listed map[string][]string
	refs   map[string]int

	dlLock  sync.Mutex
	dlTasks map[string]*dlTask
	results map[string]*dlStatus

	hostLock sync.Mutex
	hostSem  map[string]chan struct{}
}

// dlTask represents an in-flight download of an item.
type dlTask struct {
	// done is closed when the download finishes.
	done chan struct{}

	// uncached is the path of the downloaded file that is too large to
	// be cached.  It is set before done is closed, and removed when
	// all the holders of the task released it.
	uncached string

	// refs counts the holders of the task: the downloading goroutine
	// and Get callers waiting for it.  It is guarded by Cacher.dlLock.
	refs int
}

// dlStatus is the HTTP status code of a finished download.
//
// It is referenced by pointer so that the timer to invalidate it
// does not remove a newer one.
type dlStatus struct {
	code int
}

// NewCacher constructs Cacher.
func NewCacher(config *Config) (*Cacher, error) {
	if config.CheckInterval == 0 {
		return nil, errors.New("invaild check_interval")
	}
	checkInterval := time.Duration(config.CheckInterval) * time.Second
	cachePeriod := time.Duration(config.CachePeriod) * time.Second

	metaDir := filepath.Clean(config.MetaDirectory)
	if !filepath.IsAbs(metaDir) {
		return nil, errors.New("meta_dir must be an absolute path")
	}

	cacheDir := filepath.Clean(config.CacheDirectory)
	if !filepath.IsAbs(cacheDir) {
		return nil, errors.New("cache_dir must be an absolute path")
	}

	if metaDir == cacheDir {
		return nil, errors.New("meta_dir and cache_dir must be different")
	}

	if config.CacheCapacity <= 0 {
		return nil, errors.New("cache_capacity must be > 0")
	}
	capacity := uint64(config.CacheCapacity) * gib

	if config.MaxConns < 0 {
		return nil, errors.New("max_conns must be >= 0")
	}

	meta, err := NewStorage(metaDir, 0)
	if err != nil {
		return nil, errors.Wrap(err, "meta_dir")
	}
	cache, err := NewStorage(cacheDir, capacity)
	if err != nil {
		return nil, errors.Wrap(err, "cache_dir")
	}

	if err := meta.Load(); err != nil {
		return nil, errors.Wrap(err, "meta.Load")
	}
	if err := cache.Load(); err != nil {
		return nil, errors.Wrap(err, "cache.Load")
	}

	um := make(URLMap)
	for prefix, urlString := range config.Mapping {
		u, err := url.Parse(urlString)
		if err != nil {
			return nil, errors.Wrap(err, prefix)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, errors.New("unsupported scheme: " + u.Scheme)
		}
		err = um.Register(prefix, u)
		if err != nil {
			return nil, errors.Wrap(err, prefix)
		}
	}

	c := &Cacher{
		meta:          meta,
		items:         cache,
		um:            um,
		checkInterval: checkInterval,
		cachePeriod:   cachePeriod,
		client:        &http.Client{Transport: newTransport(config.MaxConns)},
		maxConns:      config.MaxConns,
		stallTimeout:  stallTimeout,
		info:          make(map[string]*apt.FileInfo),
		aliases:       make(map[string]*apt.FileInfo),
		listed:        make(map[string][]string),
		refs:          make(map[string]int),
		dlTasks:       make(map[string]*dlTask),
		results:       make(map[string]*dlStatus),
		hostSem:       make(map[string]chan struct{}),
	}

	metas := meta.ListAll()
	for _, fi := range metas {
		f, err := meta.Lookup(fi)
		if err != nil {
			return nil, errors.Wrap(err, "meta.Lookup")
		}
		t := strings.SplitN(fi.Path(), "/", 2)
		if len(t) != 2 {
			panic("there should always be a prefix!")
		}
		fil, d, err := apt.ExtractFileInfo(t[1], f)
		_ = f.Close()
		if err != nil {
			return nil, errors.Wrap(err, "ExtractFileInfo("+fi.Path()+")")
		}
		fil = addPrefix(t[0], fil)
		var aliases map[string]*apt.FileInfo
		if apt.SupportByHash(d) {
			aliases = byHashAliases(fil)
		}
		c.updateListed(fi.Path(), fil, aliases)
	}

	// add meta files w/o checksums (Release, Release.gpg, and InRelease).
	for _, fi := range metas {
		p := fi.Path()
		if _, ok := c.info[p]; !ok {
			c.info[p] = fi
			c.maintMeta(p)
		}
	}

	return c, nil
}

// newTransport returns an http.Transport that keeps up to maxConns idle
// connections per upstream host so that concurrent downloads can reuse
// them instead of reconnecting.
func newTransport(maxConns int) *http.Transport {
	var transport *http.Transport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone()
	} else {
		transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		}
	}
	if maxConns > 0 {
		transport.MaxIdleConnsPerHost = maxConns
	}
	return transport
}

// acquireSemaphore waits for a slot to connect to host.
// It returns ctx.Err() if ctx is canceled meanwhile.
func (c *Cacher) acquireSemaphore(ctx context.Context, host string) error {
	if c.maxConns == 0 {
		return ctx.Err()
	}

	c.hostLock.Lock()
	sem, ok := c.hostSem[host]
	if !ok {
		sem = make(chan struct{}, c.maxConns)
		for i := 0; i < c.maxConns; i++ {
			sem <- struct{}{}
		}
		c.hostSem[host] = sem
	}
	c.hostLock.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sem:
		return nil
	}
}

// releaseSemaphore releases the slot acquired by acquireSemaphore.
func (c *Cacher) releaseSemaphore(host string) {
	if c.maxConns == 0 {
		return
	}

	c.hostLock.Lock()
	c.hostSem[host] <- struct{}{}
	c.hostLock.Unlock()
}

func (c *Cacher) maintMeta(p string) {
	switch path.Base(p) {
	case "Release":
		well.Go(func(ctx context.Context) error {
			c.maintRelease(ctx, p, true)
			return nil
		})
	case "InRelease":
		well.Go(func(ctx context.Context) error {
			c.maintRelease(ctx, p, false)
			return nil
		})
	}
}

func (c *Cacher) maintRelease(ctx context.Context, p string, withGPG bool) {
	ticker := time.NewTicker(c.checkInterval)
	defer ticker.Stop()

	if log.Enabled(log.LvDebug) {
		log.Debug("maintRelease", map[string]interface{}{
			"path": p,
		})
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ch := c.Download(p, nil)
			if ch == nil {
				// the prefix of p was removed from the mapping.
				log.Warn("stop maintaining unmapped meta data", map[string]interface{}{
					"path": p,
				})
				return
			}
			chs := []<-chan struct{}{ch}
			if withGPG {
				chs = append(chs, c.Download(p+".gpg", nil))
			}
			for _, ch := range chs {
				select {
				case <-ctx.Done():
					return
				case <-ch:
				}
			}
		}
	}
}

func closeRespBody(r *http.Response) {
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
}

// Download downloads an item and caches it.
//
// If valid is not nil, the downloaded data is validated against it.
//
// The caller receives a channel that will be closed when the item
// is downloaded and cached.  If prefix of p is not registered
// in URLMap, nil is returned.
//
// Note that download may fail, or just invalidated soon.
// Users of this method should retry if the item is not cached
// or invalidated.
func (c *Cacher) Download(p string, valid *apt.FileInfo) <-chan struct{} {
	c.dlLock.Lock()
	defer c.dlLock.Unlock()

	t := c.startDownload(p, p, valid)
	if t == nil {
		return nil
	}
	return t.done
}

// startDownload starts downloading p unless it is already in progress,
// and returns the task for it.  The downloaded item is stored at dest,
// which differs from p if p is a by-hash path.  If prefix of p is not
// registered in URLMap, nil is returned.
//
// c.dlLock must be held.
func (c *Cacher) startDownload(p, dest string, valid *apt.FileInfo) *dlTask {
	u := c.um.URL(p)
	if u == nil {
		return nil
	}

	t, ok := c.dlTasks[p]
	if ok {
		return t
	}

	// the downloading goroutine holds the task until it finishes.
	t = &dlTask{done: make(chan struct{}), refs: 1}
	c.dlTasks[p] = t
	well.Go(func(ctx context.Context) error {
		c.download(ctx, p, dest, u, valid, t)
		return nil
	})
	return t
}

// releaseTask releases t, and removes its uncached file if t is
// no longer held by anyone.
func (c *Cacher) releaseTask(t *dlTask) {
	c.dlLock.Lock()
	t.refs--
	refs := t.refs
	c.dlLock.Unlock()

	if refs == 0 && t.uncached != "" {
		_ = os.Remove(t.uncached)
	}
}

// storage returns the Storage for the item at p.
func (c *Cacher) storage(p string) *Storage {
	if apt.IsMeta(p) {
		return c.meta
	}
	return c.items
}

// target returns the path where the item at p is stored, and FileInfo
// to validate it if known.  For a by-hash path, the path of the file
// it refers is returned so that the file is cached and parsed as such.
//
// c.fiLock must be held.
func (c *Cacher) target(p string) (string, *apt.FileInfo) {
	if fi, ok := c.aliases[p]; ok {
		return fi.Path(), fi
	}
	return p, c.info[p]
}

// download is a goroutine to download p and store it at dest.
func (c *Cacher) download(ctx context.Context, p, dest string, u *url.URL, valid *apt.FileInfo, t *dlTask) {
	statusCode := http.StatusInternalServerError

	defer func() {
		status := &dlStatus{code: statusCode}
		c.dlLock.Lock()
		delete(c.dlTasks, p)
		c.results[p] = status
		c.dlLock.Unlock()
		close(t.done)
		c.releaseTask(t)

		// invalidate result cache after some interval
		well.Go(func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(c.cachePeriod):
			}
			c.dlLock.Lock()
			// keep the result of a newer download.
			if c.results[p] == status {
				delete(c.results, p)
			}
			c.dlLock.Unlock()
			return nil
		})
	}()

	// ctx is canceled only on shutdown.
	if err := c.acquireSemaphore(ctx, u.Host); err != nil {
		return
	}
	defer c.releaseSemaphore(u.Host)

	// imitation apt-get command
	// NOTE: apt-get sets If-Modified-Since and makes a request to the server,
	// but the current aptutil cannot handle this because it cold-starts every time.
	header := http.Header{}
	header.Add("Cache-Control", "max-age=0")
	header.Add("User-Agent", "Debian APT-HTTP/1.3 (aptutil)")

	req := &http.Request{
		Method:     "GET",
		URL:        u,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
	}
	reqCtx, watcher := stall.Watch(ctx, c.stallTimeout)
	defer watcher.Stop()
	resp, err := c.client.Do(req.WithContext(reqCtx))
	if err != nil {
		err = stall.Error(reqCtx, err)
		log.Warn("GET failed", map[string]interface{}{
			"url":   u.String(),
			"error": err.Error(),
		})
		if errors.Is(err, stall.ErrStalled) {
			statusCode = http.StatusGatewayTimeout
		}
		return
	}

	// the watcher must be stopped after the body is closed.
	defer closeRespBody(resp)
	resp.Body = watcher.Wrap(resp.Body)
	statusCode = resp.StatusCode
	if statusCode != http.StatusOK {
		return
	}

	// The upstream responded with 200, but the item is not cached until
	// all the steps below succeed.  Report failures as 502 so that Get
	// does not mistake them for success and retry immediately.
	statusCode = http.StatusBadGateway

	storage := c.storage(dest)
	tempfile, err := storage.TempFile()
	if err != nil {
		log.Warn("GET failed", map[string]interface{}{
			"url":   u.String(),
			"error": err.Error(),
		})
		return
	}
	keepTempfile := false
	defer func() {
		_ = tempfile.Close()
		if !keepTempfile {
			_ = os.Remove(tempfile.Name())
		}
	}()

	fi, err := apt.CopyWithFileInfo(tempfile, resp.Body, dest)
	if err != nil {
		err = stall.Error(reqCtx, err)
		log.Warn("GET failed", map[string]interface{}{
			"url":   u.String(),
			"error": err.Error(),
		})
		if errors.Is(err, stall.ErrStalled) {
			statusCode = http.StatusGatewayTimeout
		}
		return
	}
	err = tempfile.Sync()
	if err != nil {
		log.Warn("tempfile.Sync failed", map[string]interface{}{
			"url":   u.String(),
			"error": err.Error(),
		})
		return
	}
	if valid != nil && !valid.Same(fi) {
		log.Warn("downloaded data is not valid", map[string]interface{}{
			"url": u.String(),
		})
		return
	}

	var fil []*apt.FileInfo
	var aliases map[string]*apt.FileInfo
	parsed := false

	if t := strings.SplitN(path.Clean(dest), "/", 2); len(t) == 2 && apt.IsMeta(t[1]) {
		_, err = tempfile.Seek(0, io.SeekStart)
		if err != nil {
			log.Error("failed to reset tempfile offset", map[string]interface{}{
				"error": err.Error(),
			})
			return
		}

		var d apt.Paragraph
		fil, d, err = apt.ExtractFileInfo(t[1], tempfile)
		if err != nil {
			log.Error("invalid meta data", map[string]interface{}{
				"path":  dest,
				"error": err.Error(),
			})
			// do not return; we accept broken meta data as is.
		} else {
			parsed = true
		}
		fil = addPrefix(t[0], fil)
		if parsed && apt.SupportByHash(d) {
			aliases = byHashAliases(fil)
		}
	}

	c.fiLock.Lock()
	defer c.fiLock.Unlock()

	// To keep consistency between Cacher and Storage so that
	// both have the same set of FileInfo, storage.Insert need to be
	// guarded by c.fiLock.
	err = storage.Insert(tempfile.Name(), fi)
	switch {
	case errors.Is(err, ErrTooLarge):
		// Hand the downloaded file over to the waiters in Get
		// without caching it.
		log.Warn("serving an item without caching as it exceeds cache_capacity", map[string]interface{}{
			"path": dest,
			"size": fi.Size(),
		})
		t.uncached = tempfile.Name()
		keepTempfile = true
		statusCode = http.StatusOK
		return
	case err != nil:
		// Storage stays consistent with c.info even if Insert fails;
		// the item is just not cached and will be downloaded again.
		log.Error("could not save an item", map[string]interface{}{
			"path":  dest,
			"error": err.Error(),
		})
		statusCode = http.StatusInternalServerError
		return
	}

	if parsed {
		c.updateListed(dest, fil, aliases)
	}
	switch {
	case apt.IsMeta(dest):
		_, ok := c.info[dest]
		if !ok {
			// As this is the first time that downloaded meta file dest,
			c.maintMeta(dest)
		}
		c.info[dest] = fi
	case c.refs[dest] > 0:
		c.info[dest] = fi
	}
	// Other items are not kept in c.info so that it does not grow
	// without bound; Get looks them up in Storage by path.
	statusCode = http.StatusOK
	log.Info("downloaded and cached", map[string]interface{}{
		"path": p,
	})
}

// updateListed registers fil as the items and aliases as the by-hash
// paths listed in the meta data file p, replacing those registered for
// the previous version of p.  Items and by-hash paths no longer listed
// in any meta data file are removed from c.info and c.aliases.
//
// c.fiLock must be held for writing unless c is under construction.
func (c *Cacher) updateListed(p string, fil []*apt.FileInfo, aliases map[string]*apt.FileInfo) {
	paths := make([]string, 0, len(fil)+len(aliases))
	for _, fi := range fil {
		paths = append(paths, fi.Path())
		c.info[fi.Path()] = fi
	}
	for hp, fi := range aliases {
		paths = append(paths, hp)
		c.aliases[hp] = fi
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)

	for _, fp := range paths {
		c.refs[fp]++
	}
	for _, fp := range c.listed[p] {
		c.refs[fp]--
		if c.refs[fp] > 0 {
			continue
		}
		delete(c.refs, fp)
		delete(c.info, fp)
		delete(c.aliases, fp)
	}

	if len(paths) == 0 {
		delete(c.listed, p)
		return
	}
	c.listed[p] = paths
}

// lookup looks up the cached item for p.
//
// It returns the path where the item is stored, FileInfo to validate
// the item if known, and the cache file.  If the item is not cached,
// the returned os.File is nil.
func (c *Cacher) lookup(p string) (dest string, valid *apt.FileInfo, f *os.File, err error) {
	c.fiLock.RLock()
	dest, valid = c.target(p)
	c.fiLock.RUnlock()

	storage := c.storage(dest)
	if valid != nil {
		f, err = storage.Lookup(valid)
	} else {
		f, err = storage.Open(dest)
	}
	if errors.Is(err, ErrNotFound) {
		return dest, valid, nil, nil
	}
	return dest, valid, f, err
}

// checkPath returns http.StatusNotFound if p is not to be served,
// or http.StatusOK otherwise.
func (c *Cacher) checkPath(p string) int {
	if c.um.URL(p) == nil {
		return http.StatusNotFound
	}
	if apt.IsMeta(p) && !apt.IsSupported(p) {
		// return 404 for unsupported compression algorithms
		return http.StatusNotFound
	}
	return http.StatusOK
}

// Get looks up a cached item, and if not found, downloads it
// from the upstream server.
//
// The return values are cached HTTP status code of the response from
// an upstream server, a pointer to os.File for the cache file,
// and error.  If the item is too large to be cached, the returned
// os.File is the downloaded file that is not cached.
//
// If ctx is canceled while waiting for the download, Get returns
// ctx.Err().  The download itself continues for other callers.
func (c *Cacher) Get(ctx context.Context, p string) (statusCode int, f *os.File, err error) {
	if status := c.checkPath(p); status != http.StatusOK {
		return status, nil, nil
	}

RETRY:
	dest, fi, f, err := c.lookup(p)
	switch {
	case err != nil:
		log.Error("lookup failure", map[string]interface{}{
			"error": err.Error(),
		})
		return http.StatusInternalServerError, nil, err
	case f != nil:
		return http.StatusOK, f, nil
	}

	// not found in storage.
	c.dlLock.Lock()
	if result, ok := c.results[p]; ok && result.code != http.StatusOK {
		c.dlLock.Unlock()
		return result.code, nil, nil
	}
	t := c.startDownload(p, dest, fi)
	t.refs++
	c.dlLock.Unlock()

	select {
	case <-ctx.Done():
		c.releaseTask(t)
		return http.StatusServiceUnavailable, nil, ctx.Err()
	case <-t.done:
	}

	if t.uncached == "" {
		c.releaseTask(t)
		goto RETRY
	}

	// The file is opened before releasing t so that it is not
	// removed meanwhile.
	f, err = os.Open(t.uncached)
	c.releaseTask(t)
	if err != nil {
		log.Error("failed to open an uncached item", map[string]interface{}{
			"path":  p,
			"error": err.Error(),
		})
		return http.StatusInternalServerError, nil, err
	}
	return http.StatusOK, f, nil
}

// Head returns the HTTP status code and the size of the item for p
// like Get, but does not download the item if it is not cached.
// Instead, it asks the upstream server with a HEAD request.
//
// The returned size is -1 if it is unknown.
func (c *Cacher) Head(ctx context.Context, p string) (statusCode int, size int64, err error) {
	if status := c.checkPath(p); status != http.StatusOK {
		return status, -1, nil
	}

	_, _, f, err := c.lookup(p)
	switch {
	case err != nil:
		log.Error("lookup failure", map[string]interface{}{
			"error": err.Error(),
		})
		return http.StatusInternalServerError, -1, err
	case f != nil:
		defer func() { _ = f.Close() }()
		stat, err := f.Stat()
		if err != nil {
			log.Error("failed to stat a cached item", map[string]interface{}{
				"path":  p,
				"error": err.Error(),
			})
			return http.StatusInternalServerError, -1, err
		}
		return http.StatusOK, stat.Size(), nil
	}

	c.dlLock.Lock()
	result, ok := c.results[p]
	c.dlLock.Unlock()
	if ok && result.code != http.StatusOK {
		return result.code, -1, nil
	}

	u := c.um.URL(p)
	if err := c.acquireSemaphore(ctx, u.Host); err != nil {
		return http.StatusServiceUnavailable, -1, err
	}
	defer c.releaseSemaphore(u.Host)

	// a response to HEAD has no body to watch.
	reqCtx, cancel := context.WithTimeout(ctx, c.stallTimeout)
	defer cancel()

	header := http.Header{}
	header.Add("Cache-Control", "max-age=0")
	header.Add("User-Agent", "Debian APT-HTTP/1.3 (aptutil)")

	req := &http.Request{
		Method:     "HEAD",
		URL:        u,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
	}
	resp, err := c.client.Do(req.WithContext(reqCtx))
	if err != nil {
		log.Warn("HEAD failed", map[string]interface{}{
			"url":   u.String(),
			"error": err.Error(),
		})
		switch {
		case ctx.Err() != nil:
			return http.StatusServiceUnavailable, -1, ctx.Err()
		case reqCtx.Err() != nil:
			return http.StatusGatewayTimeout, -1, nil
		}
		return http.StatusInternalServerError, -1, nil
	}
	closeRespBody(resp)

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, -1, nil
	}
	return http.StatusOK, resp.ContentLength, nil
}
