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
	"github.com/cybozu-go/log"
	"github.com/cybozu-go/well"
	"github.com/pkg/errors"
)

const (
	gib            = 1 << 30
	requestTimeout = 30 * time.Minute
)

// addPrefix add prefix for each *FileInfo in fil.
func addPrefix(prefix string, fil []*apt.FileInfo) []*apt.FileInfo {
	ret := make([]*apt.FileInfo, 0, len(fil))
	for _, fi := range fil {
		ret = append(ret, fi.AddPrefix(prefix))
	}
	return ret
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

	fiLock sync.RWMutex
	info   map[string]*apt.FileInfo
	// listed maps a meta data file to the items it lists, and refs counts
	// how many meta data files list each item.  They are used to remove
	// stale entries from info when meta data files are updated.
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
		info:          make(map[string]*apt.FileInfo),
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
		fil, _, err := apt.ExtractFileInfo(t[1], f)
		_ = f.Close()
		if err != nil {
			return nil, errors.Wrap(err, "ExtractFileInfo("+fi.Path()+")")
		}
		c.updateListed(fi.Path(), addPrefix(t[0], fil))
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

func (c *Cacher) acquireSemaphore(host string) {
	if c.maxConns == 0 {
		return
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

	<-sem
}

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

	t := c.startDownload(p, valid)
	if t == nil {
		return nil
	}
	return t.done
}

// startDownload starts downloading p unless it is already in progress,
// and returns the task for it.  If prefix of p is not registered
// in URLMap, nil is returned.
//
// c.dlLock must be held.
func (c *Cacher) startDownload(p string, valid *apt.FileInfo) *dlTask {
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
		c.download(ctx, p, u, valid, t)
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

// download is a goroutine to download an item.
func (c *Cacher) download(ctx context.Context, p string, u *url.URL, valid *apt.FileInfo, t *dlTask) {
	c.acquireSemaphore(u.Host)

	statusCode := http.StatusInternalServerError

	defer func() {
		c.releaseSemaphore(u.Host)
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

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

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
	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		log.Warn("GET failed", map[string]interface{}{
			"url":   u.String(),
			"error": err.Error(),
		})
		return
	}

	defer closeRespBody(resp)
	statusCode = resp.StatusCode
	if statusCode != http.StatusOK {
		return
	}

	// The upstream responded with 200, but the item is not cached until
	// all the steps below succeed.  Report failures as 502 so that Get
	// does not mistake them for success and retry immediately.
	statusCode = http.StatusBadGateway

	storage := c.items
	if apt.IsMeta(p) {
		storage = c.meta
	}

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

	fi, err := apt.CopyWithFileInfo(tempfile, resp.Body, p)
	if err != nil {
		log.Warn("GET failed", map[string]interface{}{
			"url":   u.String(),
			"error": err.Error(),
		})
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
	parsed := false

	if t := strings.SplitN(path.Clean(p), "/", 2); len(t) == 2 && apt.IsMeta(t[1]) {
		_, err = tempfile.Seek(0, io.SeekStart)
		if err != nil {
			log.Error("failed to reset tempfile offset", map[string]interface{}{
				"error": err.Error(),
			})
			return
		}

		fil, _, err = apt.ExtractFileInfo(t[1], tempfile)
		if err != nil {
			log.Error("invalid meta data", map[string]interface{}{
				"path":  p,
				"error": err.Error(),
			})
			// do not return; we accept broken meta data as is.
		} else {
			parsed = true
		}
		fil = addPrefix(t[0], fil)
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
			"path": p,
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
			"path":  p,
			"error": err.Error(),
		})
		statusCode = http.StatusInternalServerError
		return
	}

	if parsed {
		c.updateListed(p, fil)
	}
	if apt.IsMeta(p) {
		_, ok := c.info[p]
		if !ok {
			// As this is the first time that downloaded meta file p,
			c.maintMeta(p)
		}
	}
	c.info[p] = fi
	statusCode = http.StatusOK
	log.Info("downloaded and cached", map[string]interface{}{
		"path": p,
	})
}

// updateListed registers fil as the items listed in the meta data file p,
// replacing the items registered for the previous version of p.
// Items no longer listed in any meta data file are removed from c.info.
//
// c.fiLock must be held for writing unless c is under construction.
func (c *Cacher) updateListed(p string, fil []*apt.FileInfo) {
	paths := make([]string, 0, len(fil))
	for _, fi := range fil {
		paths = append(paths, fi.Path())
		c.info[fi.Path()] = fi
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
	}

	if len(paths) == 0 {
		delete(c.listed, p)
		return
	}
	c.listed[p] = paths
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
	u := c.um.URL(p)
	if u == nil {
		return http.StatusNotFound, nil, nil
	}

	storage := c.items
	if apt.IsMeta(p) {
		if !apt.IsSupported(p) {
			// return 404 for unsupported compression algorithms
			return http.StatusNotFound, nil, nil
		}
		storage = c.meta
	}

RETRY:
	c.fiLock.RLock()
	fi, ok := c.info[p]
	c.fiLock.RUnlock()

	if ok {
		f, err := storage.Lookup(fi)
		switch {
		case err == nil:
			return http.StatusOK, f, nil
		case errors.Is(err, ErrNotFound):
		default:
			log.Error("lookup failure", map[string]interface{}{
				"error": err.Error(),
			})
			return http.StatusInternalServerError, nil, err
		}
	}

	// not found in storage.
	c.dlLock.Lock()
	if result, ok := c.results[p]; ok && result.code != http.StatusOK {
		c.dlLock.Unlock()
		return result.code, nil, nil
	}
	t := c.startDownload(p, fi)
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
