package mirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"github.com/cybozu-go/aptutil/apt"
	"github.com/cybozu-go/aptutil/internal/stall"
	"github.com/cybozu-go/log"
	"github.com/cybozu-go/well"
	"github.com/pkg/errors"
)

const (
	timestampFormat  = "20060102_150405"
	progressInterval = 5 * time.Minute
	httpRetries      = 5

	// stallTimeout is how long a download may make no progress, either
	// waiting for the response header or reading the body, before it
	// is aborted and retried.
	stallTimeout = 1 * time.Minute
)

var validID = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Mirror implements mirroring logics.
type Mirror struct {
	id      string
	dir     string
	mc      *MirrConfig
	storage *Storage
	current *Storage

	// semaphore limits concurrent downloads.  nil means no limit.
	semaphore chan struct{}
	client    *http.Client

	// stallTimeout aborts a download that makes no progress for it.
	stallTimeout time.Duration
}

// NewMirror constructs a Mirror for given mirror id.
func NewMirror(t time.Time, id string, c *Config) (*Mirror, error) {
	dir := filepath.Clean(c.Dir)
	mc, ok := c.Mirrors[id]
	if !ok {
		return nil, errors.New("no such mirror: " + id)
	}

	// sanity checks
	if !validID.MatchString(id) {
		return nil, errors.New("invalid id: " + id)
	}
	if err := mc.Check(); err != nil {
		return nil, errors.Wrap(err, id)
	}
	if c.MaxConns < 0 {
		return nil, errors.New("max_conns must be >= 0")
	}

	var currentStorage *Storage
	curdir, err := filepath.EvalSymlinks(filepath.Join(dir, id))
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, errors.Wrap(err, id)
	default:
		currentStorage, err = NewStorage(filepath.Dir(curdir), id)
		if err != nil {
			return nil, errors.Wrap(err, id)
		}
		err = currentStorage.Load()
		if err != nil {
			return nil, errors.Wrap(err, id)
		}
	}

	d := filepath.Join(dir, "."+id+"."+t.Format(timestampFormat))
	err = os.Mkdir(d, 0o755)
	if err != nil {
		return nil, errors.Wrap(err, id)
	}
	storage, err := NewStorage(d, id)
	if err != nil {
		return nil, errors.Wrap(err, id)
	}

	// zero disables limit on the number of connections.
	var sem chan struct{}
	if c.MaxConns > 0 {
		sem = make(chan struct{}, c.MaxConns)
		for i := 0; i < c.MaxConns; i++ {
			sem <- struct{}{}
		}
	}

	transport := clonedTransport(http.DefaultTransport)
	if transport == nil {
		transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		}
	}
	if c.MaxConns > 0 {
		transport.MaxIdleConnsPerHost = c.MaxConns
	}

	mr := &Mirror{
		id:        id,
		dir:       dir,
		mc:        mc,
		storage:   storage,
		current:   currentStorage,
		semaphore: sem,
		client: &http.Client{
			Transport: transport,
		},
		stallTimeout: stallTimeout,
	}
	return mr, nil
}

func clonedTransport(rt http.RoundTripper) *http.Transport {
	t, ok := rt.(*http.Transport)
	if !ok {
		return nil
	}
	return t.Clone()
}

// acquireSemaphore waits for a slot to download an item.
func (m *Mirror) acquireSemaphore(ctx context.Context) error {
	if m.semaphore == nil {
		return ctx.Err()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.semaphore:
		return nil
	}
}

// releaseSemaphore releases the slot acquired by acquireSemaphore.
func (m *Mirror) releaseSemaphore() {
	if m.semaphore != nil {
		m.semaphore <- struct{}{}
	}
}

func (m *Mirror) storeLink(fi *apt.FileInfo, fp string, byhash bool) error {
	if byhash {
		return m.storage.StoreLinkWithHash(fi, fp)
	}
	return m.storage.StoreLink(fi, fp)
}

func (m *Mirror) extractItems(indices []*apt.FileInfo, indexMap, itemMap map[string]*apt.FileInfo, byhash bool) error {
	for _, index := range indices {
		p := index.Path()
		if !m.mc.MatchingIndex(p) || !apt.IsSupported(p) {
			continue
		}
		openPath := p
		if hp := byHashPath(index); byhash && hp != "" {
			openPath = hp
		}
		f, err := m.storage.Open(openPath)
		if err != nil {
			return err
		}

		fil, _, err := apt.ExtractFileInfo(p, f)
		_ = f.Close()
		if err != nil {
			return err
		}

		for _, fi := range fil {
			fipath := fi.Path()
			if _, ok := indexMap[fipath]; ok {
				// already included in Release/InRelease
				continue
			}
			itemMap[fipath] = fi
		}
	}
	return nil
}

func (m *Mirror) replaceLink() error {
	tname := filepath.Join(m.dir, m.id+".tmp")
	_ = os.Remove(tname)
	err := os.Symlink(filepath.Join(m.storage.Dir(), m.id), tname)
	if err != nil {
		return err
	}

	// symlink exists only in dentry
	err = DirSync(m.dir)
	if err != nil {
		return err
	}

	err = os.Rename(tname, filepath.Join(m.dir, m.id))
	if err != nil {
		return err
	}

	return DirSync(m.dir)
}

// Update updates mirrored files.
func (m *Mirror) Update(ctx context.Context) error {
	itemMap := make(map[string]*apt.FileInfo)

	for _, suite := range m.mc.Suites {
		err := m.updateSuite(ctx, suite, itemMap)
		if err != nil {
			return err
		}
	}

	// download all files matching the configuration.
	log.Info("download items", map[string]interface{}{
		"repo":  m.id,
		"items": len(itemMap),
	})
	_, err := m.downloadItems(ctx, itemMap)
	if err != nil {
		return errors.Wrap(err, m.id)
	}

	// all files are downloaded (or reused)
	log.Info("saving meta data", map[string]interface{}{
		"repo": m.id,
	})
	err = m.storage.Save()
	if err != nil {
		return errors.Wrap(err, m.id)
	}

	// replace the symlink atomically
	err = m.replaceLink()
	if err != nil {
		return errors.Wrap(err, m.id)
	}

	log.Info("update succeeded", map[string]interface{}{
		"repo": m.id,
	})
	return nil
}

// updateSuite partially updates mirror for a suite.
func (m *Mirror) updateSuite(ctx context.Context, suite string, itemMap map[string]*apt.FileInfo) error {
	log.Info("download Release/InRelease", map[string]interface{}{
		"repo":  m.id,
		"suite": suite,
	})
	indexMap, byhash, err := m.downloadRelease(ctx, suite)
	if err != nil {
		return errors.Wrap(err, m.id)
	}

	if byhash {
		log.Info("detected by-hash support", map[string]interface{}{
			"repo":  m.id,
			"suite": suite,
		})
	}

	if len(indexMap) == 0 {
		return errors.New(m.id + ": found no Release/InRelease")
	}

	// WORKAROUND: some (zabbix) repositories returns wrong contents
	// for non-existent files such as Sources (looks like the body of
	// Sources.gz is returned).
	if !m.mc.Source {
		tmpMap := make(map[string]*apt.FileInfo)
		for p, fi := range indexMap {
			base := path.Base(p)
			base = base[0 : len(base)-len(path.Ext(base))]
			if base == "Sources" {
				continue
			}
			tmpMap[p] = fi
		}
		indexMap = tmpMap
	}

	// download (or reuse) all indices
	indices, err := m.downloadIndices(ctx, indexMap, byhash)
	if err != nil {
		return errors.Wrap(err, m.id)
	}

	// extract file information from indices
	err = m.extractItems(indices, indexMap, itemMap, byhash)
	if err != nil {
		return errors.Wrap(err, m.id)
	}
	return nil
}

type dlResult struct {
	status   int
	path     string
	fi       *apt.FileInfo
	tempfile *os.File
	err      error
}

func closeRespBody(r *http.Response) {
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
}

// localError is an error of the local file system.  Unlike errors of
// the upstream server or the network, retrying the download does not
// fix it.
type localError struct {
	err error
}

func (e *localError) Error() string {
	return e.err.Error()
}

func (e *localError) Unwrap() error {
	return e.err
}

// localWriter wraps errors of w by localError so that they are told
// from errors reading the response body.
type localWriter struct {
	w io.Writer
}

func (lw localWriter) Write(p []byte) (int, error) {
	n, err := lw.w.Write(p)
	if err != nil {
		err = &localError{err}
	}
	return n, err
}

func closeAndRemoveFile(f *os.File) {
	_ = f.Close()
	_ = os.Remove(f.Name())
}

// download is a goroutine to download an item.
func (m *Mirror) download(ctx context.Context,
	p string, fi *apt.FileInfo, byhash bool, ch chan<- *dlResult,
) {
	var tempfile *os.File
	var resp *http.Response
	var watcher *stall.Watcher
	r := &dlResult{
		path: p,
	}

	defer func() {
		if resp != nil {
			closeRespBody(resp)
		}
		if watcher != nil {
			watcher.Stop()
		}
		r.tempfile = tempfile
		ch <- r
		m.releaseSemaphore()
	}()

	var retries uint
	targets := []string{p}
	if byhash && fi != nil {
		targets = append(targets, byHashPaths(fi)...)
	}

RETRY:
	if tempfile != nil {
		closeAndRemoveFile(tempfile)
		tempfile = nil
	}
	// close the previous response so that its connection can be reused.
	if resp != nil {
		closeRespBody(resp)
		resp = nil
	}
	if watcher != nil {
		watcher.Stop()
		watcher = nil
	}

	// allow interrupts
	select {
	case <-ctx.Done():
		r.err = ctx.Err()
		return
	default:
	}

	if retries > 0 {
		log.Warn("retrying download", map[string]interface{}{
			"repo": m.id,
			"path": p,
		})
		select {
		case <-ctx.Done():
			r.err = ctx.Err()
			return
		case <-time.After(time.Duration(1<<(retries-1)) * time.Second):
		}
	}

	// imitation apt-get command
	// NOTE: apt-get sets If-Modified-Since and makes a request to the server,
	// but the current aptutil cannot handle this because it cold-starts every time.
	header := http.Header{}
	header.Add("Cache-Control", "max-age=0")
	header.Add("User-Agent", "Debian APT-HTTP/1.3 (aptutil)")

	req := &http.Request{
		Method:     "GET",
		URL:        m.mc.Resolve(targets[0]),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
	}
	reqCtx, watcher := stall.Watch(ctx, m.stallTimeout)
	resp, err := m.client.Do(req.WithContext(reqCtx))
	if err != nil {
		err = stall.Error(reqCtx, err)
		log.Warn("GET failed", map[string]interface{}{
			"repo":  m.id,
			"path":  p,
			"error": err.Error(),
		})
		if retries < httpRetries {
			retries++
			goto RETRY
		}
		r.err = err
		return
	}

	if log.Enabled(log.LvDebug) {
		log.Debug("downloaded", map[string]interface{}{
			"repo":               m.id,
			"path":               p,
			log.FnHTTPStatusCode: resp.StatusCode,
		})
	}

	resp.Body = watcher.Wrap(resp.Body)
	r.status = resp.StatusCode
	if r.status >= 500 && retries < httpRetries {
		retries++
		goto RETRY
	}

	if r.status != 200 {
		return
	}

	tempfile, err = m.storage.TempFile()
	if err != nil {
		r.err = &localError{err}
		return
	}
	fi2, err := apt.CopyWithFileInfo(localWriter{tempfile}, resp.Body, p)
	var lerr *localError
	if errors.As(err, &lerr) {
		r.err = err
		return
	}
	if err != nil {
		err = stall.Error(reqCtx, err)
		log.Warn("GET failed", map[string]interface{}{
			"repo":  m.id,
			"path":  p,
			"error": err.Error(),
		})
		if retries < httpRetries {
			retries++
			goto RETRY
		}
		r.err = err
		return
	}
	err = tempfile.Sync()
	if err != nil {
		r.err = &localError{errors.Wrap(err, "tempfile.Sync")}
		return
	}
	err = os.Chmod(tempfile.Name(), 0o644)
	if err != nil {
		r.err = &localError{errors.Wrap(err, "os.Chmod")}
		return
	}

	if fi != nil && !fi.Same(fi2) {
		if len(targets) > 1 {
			targets = targets[1:]
			log.Warn("try by-hash retrieval", map[string]interface{}{
				"repo":   m.id,
				"path":   p,
				"target": targets[0],
			})
			goto RETRY
		}
		r.err = errors.New("invalid checksum for " + p)
		return
	}

	_, err = tempfile.Seek(0, io.SeekStart)
	if err != nil {
		r.err = &localError{errors.Wrap(err, "tempfile.Seek")}
		return
	}
	r.fi = fi2
}

// releaseFile is a Release, Release.gpg, or InRelease file downloaded.
type releaseFile struct {
	path string
	fil  []*apt.FileInfo
	d    apt.Paragraph
}

// handleReleaseResults handles a result of downloading a release file.
// It returns nil if the file is not found.
func (m *Mirror) handleReleaseResults(results <-chan *dlResult) (*releaseFile, error) {
	r := <-results
	if r.tempfile != nil {
		defer closeAndRemoveFile(r.tempfile)
	}

	if r.err != nil {
		return nil, errors.Wrap(r.err, "download")
	}

	// some servers such as Amazon S3 return 403 for missing files.
	if 400 <= r.status && r.status < 500 {
		// return no error to continue
		return nil, nil
	}

	if r.status != http.StatusOK {
		return nil, fmt.Errorf("status %d for %s", r.status, r.path)
	}

	// 200 OK
	err := m.storage.StoreLink(r.fi, r.tempfile.Name())
	if err != nil {
		return nil, errors.Wrap(err, "storage.Store")
	}
	fil, d, err := apt.ExtractFileInfo(r.path, r.tempfile)
	if err != nil {
		return nil, errors.Wrap(err, "ExtractFileInfo: "+r.path)
	}

	return &releaseFile{path: r.path, fil: fil, d: d}, nil
}

// sameFileInfos returns true if filMap has exactly the files in fil.
func sameFileInfos(filMap map[string]*apt.FileInfo, fil []*apt.FileInfo) bool {
	if len(filMap) != len(fil) {
		return false
	}
	for _, fi := range fil {
		existing, ok := filMap[fi.Path()]
		if !ok || existing.Conflicts(fi) {
			return false
		}
	}
	return true
}

// downloadRelease downloads release files of a suite, and returns the
// indices listed in them and whether they support by-hash retrieval.
//
// Release and InRelease must list the same indices; otherwise they are
// of different generations as the upstream is being updated, and
// cannot make a consistent mirror.
//
// Unless allowed by the configuration, the release files must be
// signed by InRelease or Release.gpg, so that a mirror is not
// published without signatures when they fail to download.
func (m *Mirror) downloadRelease(ctx context.Context, suite string) (map[string]*apt.FileInfo, bool, error) {
	releases := m.mc.ReleaseFiles(suite)
	results := make(chan *dlResult, len(releases))

	for _, p := range releases {
		if err := m.acquireSemaphore(ctx); err != nil {
			return nil, false, err
		}

		go m.download(ctx, p, nil, false, results)
	}

	byhash := true
	found := make(map[string]bool)
	var first *releaseFile
	filMap := make(map[string]*apt.FileInfo)
	for i := 0; i < len(releases); i++ {
		rf, err := m.handleReleaseResults(results)
		if err != nil {
			return nil, false, err
		}
		if rf == nil {
			continue
		}

		base := path.Base(rf.path)
		found[base] = true
		if base == "Release.gpg" {
			continue
		}

		if byhash {
			byhash = apt.SupportByHash(rf.d)
		}
		if first == nil {
			first = rf
			for _, fi := range rf.fil {
				filMap[fi.Path()] = fi
			}
			continue
		}
		if !sameFileInfos(filMap, rf.fil) {
			return nil, false, errors.New("inconsistent indices in " + first.path + " and " + rf.path)
		}
	}

	if !found["Release"] && !found["InRelease"] {
		return nil, false, errors.New("found no Release/InRelease for " + suite)
	}
	signed := found["InRelease"] || (found["Release"] && found["Release.gpg"])
	if !signed && !m.mc.AllowUnsigned {
		return nil, false, errors.New("found neither InRelease nor Release.gpg for " + suite +
			"; set allow_unsigned to mirror unsigned repositories")
	}

	return filMap, byhash, nil
}

func (m *Mirror) downloadIndices(ctx context.Context,
	filMap map[string]*apt.FileInfo, byhash bool,
) ([]*apt.FileInfo, error) {
	fil := make([]*apt.FileInfo, 0, len(filMap))
	for _, fi := range filMap {
		fil = append(fil, fi)
	}

	log.Info("download other indices", map[string]interface{}{
		"repo":    m.id,
		"indices": len(fil),
	})

	return m.downloadFiles(ctx, fil, true, byhash)
}

func (m *Mirror) downloadItems(ctx context.Context,
	fiMap map[string]*apt.FileInfo,
) ([]*apt.FileInfo, error) {
	fil := make([]*apt.FileInfo, 0, len(fiMap))
	for _, fi := range fiMap {
		fil = append(fil, fi)
	}
	return m.downloadFiles(ctx, fil, false, false)
}

func (m *Mirror) downloadFiles(ctx context.Context,
	fil []*apt.FileInfo, allowMissing, byhash bool,
) ([]*apt.FileInfo, error) {
	results := make(chan *dlResult, len(fil))
	var reused, downloaded []*apt.FileInfo

	env := well.NewEnvironment(ctx)
	env.Go(func(ctx context.Context) error {
		var err error
		reused, err = m.reuseOrDownload(ctx, fil, byhash, results)
		return err
	})
	env.Go(func(ctx context.Context) error {
		var err error
		downloaded, err = m.recvResult(allowMissing, byhash, results)
		return err
	})
	env.Stop()
	err := env.Wait()
	if err != nil {
		return nil, err
	}

	log.Info("stats", map[string]interface{}{
		"repo":       m.id,
		"total":      len(fil),
		"reused":     len(reused),
		"downloaded": len(downloaded),
	})

	// reused has enough capacity.  See reuseOrDownload.
	return append(reused, downloaded...), nil
}

func (m *Mirror) reuseOrDownload(ctx context.Context, fil []*apt.FileInfo,
	byhash bool, results chan<- *dlResult,
) ([]*apt.FileInfo, error) {
	// environment to manage downloading goroutines.
	env := well.NewEnvironment(ctx)

	// on return, wait for all DL goroutines then signal recvResult
	// by closing results channel.
	defer func() {
		env.Stop()
		_ = env.Wait()
		close(results)
	}()

	reused := make([]*apt.FileInfo, 0, len(fil))
	loggedAt := time.Now()

	for i, fi := range fil {
		// avoid assignment
		fi := fi
		now := time.Now()
		if now.Sub(loggedAt) > progressInterval {
			loggedAt = now
			log.Info("download progress", map[string]interface{}{
				"repo":      m.id,
				"total":     len(fil),
				"reused":    len(reused),
				"downloads": i - len(reused),
			})
		}

		if m.current != nil {
			localfi, fullpath := m.current.Lookup(fi, byhash)
			if localfi != nil {
				err := m.storeLink(localfi, fullpath, byhash)
				if err != nil {
					return nil, errors.Wrap(err, "storeLink")
				}
				reused = append(reused, localfi)
				if log.Enabled(log.LvDebug) {
					log.Debug("reuse item", map[string]interface{}{
						"repo": m.id,
						"path": fi.Path(),
					})
				}
				continue
			}
		}

		if err := m.acquireSemaphore(ctx); err != nil {
			return nil, err
		}

		env.Go(func(ctx context.Context) error {
			m.download(ctx, fi.Path(), fi, byhash, results)
			return nil
		})
	}
	return reused, nil
}

func (m *Mirror) handleResult(r *dlResult, allowMissing, byhash bool) (*apt.FileInfo, error) {
	if r.tempfile != nil {
		defer closeAndRemoveFile(r.tempfile)
	}

	if r.err != nil {
		return nil, errors.Wrap(r.err, "download")
	}

	if allowMissing && r.status == http.StatusNotFound {
		log.Warn("missing file", map[string]interface{}{
			"repo": m.id,
			"path": r.path,
		})
		// return no error to continue
		return nil, nil
	}

	if r.status != http.StatusOK {
		return nil, fmt.Errorf("status %d for %s", r.status, r.path)
	}

	err := m.storeLink(r.fi, r.tempfile.Name(), byhash)
	if err != nil {
		return nil, errors.Wrap(err, "store")
	}

	return r.fi, nil
}

func (m *Mirror) recvResult(allowMissing, byhash bool, results <-chan *dlResult) ([]*apt.FileInfo, error) {
	var dlfil []*apt.FileInfo

	for r := range results {
		fi, err := m.handleResult(r, allowMissing, byhash)
		if err != nil {
			return nil, err
		}
		if fi != nil {
			dlfil = append(dlfil, fi)
		}
	}

	return dlfil, nil
}
