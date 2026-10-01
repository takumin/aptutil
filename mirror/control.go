package mirror

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cybozu-go/log"
	"github.com/cybozu-go/well"
	"github.com/pkg/errors"
)

const (
	lockFilename = ".lock"
)

// mirrorDirName matches the names of directories created by NewMirror,
// i.e. "." + id + "." + timestamp in timestampFormat.
var mirrorDirName = regexp.MustCompile(`^\.[a-z0-9_-]+\.[0-9]{8}_[0-9]{6}$`)

// updateMirrors updates mirrors independently so that a failure of
// one mirror does not stop updating the others.  It returns an error
// if any of them failed.
func updateMirrors(ctx context.Context, c *Config, mirrors []string) error {
	t := time.Now()

	var mu sync.Mutex
	var failed []string
	fail := func(id string, err error) {
		log.Error("update failed", map[string]interface{}{
			"repo":  id,
			"error": err.Error(),
		})
		mu.Lock()
		failed = append(failed, id)
		mu.Unlock()
	}

	var ml []*Mirror
	for _, id := range mirrors {
		m, err := NewMirror(t, id, c)
		if err != nil {
			fail(id, err)
			continue
		}
		ml = append(ml, m)
	}

	log.Info("update starts", nil)

	// run goroutines in an environment.
	env := well.NewEnvironment(ctx)

	for _, m := range ml {
		env.Go(func(ctx context.Context) error {
			if err := m.Update(ctx); err != nil {
				fail(m.id, err)
			}
			return nil
		})
	}
	env.Stop()
	err := env.Wait()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(failed) > 0 {
		sort.Strings(failed)
		return errors.Errorf("failed to update %d of %d mirrors: %s",
			len(failed), len(mirrors), strings.Join(failed, ", "))
	}

	log.Info("update ends", nil)
	return nil
}

// gc removes old mirror directories, if any.
//
// Only directories created by NewMirror are removed so that other files
// in c.Dir, such as those put by the administrator, are kept.
func gc(ctx context.Context, c *Config) error {
	using := make(map[string]bool)

	dentries, err := os.ReadDir(c.Dir)
	if err != nil {
		return err
	}

	// search symlinks and its pointing directories
	for _, dentry := range dentries {
		if (dentry.Type() & os.ModeSymlink) == 0 {
			continue
		}
		using[dentry.Name()] = true
		p, err := filepath.EvalSymlinks(filepath.Join(c.Dir, dentry.Name()))
		if err != nil {
			// a broken symlink should not stop removing other old mirrors.
			log.Warn("gc: failed to resolve a symlink", map[string]interface{}{
				"path":  filepath.Join(c.Dir, dentry.Name()),
				"error": err.Error(),
			})
			continue
		}
		using[filepath.Base(filepath.Dir(p))] = true
	}

	// remove unused mirror directories.
	for _, dentry := range dentries {
		if using[dentry.Name()] || !dentry.IsDir() || !mirrorDirName.MatchString(dentry.Name()) {
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		p := filepath.Join(c.Dir, dentry.Name())
		log.Info("removing old mirror", map[string]interface{}{
			"path": p,
		})
		err := os.RemoveAll(p)
		if err != nil {
			return errors.Wrap(err, "gc")
		}
	}

	return nil
}

// Run starts mirroring.
//
// The first thing to do is to acquire flock on the lock file.
//
// mirrors is a list of mirror IDs defined in the configuration file
// (or keys in c.Mirrors).  If mirrors is an empty list, all mirrors
// will be updated.
func Run(c *Config, mirrors []string) error {
	lockFile := filepath.Join(c.Dir, lockFilename)
	f, err := os.Open(lockFile) //nolint:gosec // G304: lock file lives in the configured mirror directory
	switch {
	case os.IsNotExist(err):
		f2, err := os.OpenFile(lockFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G304: lock file lives in the configured mirror directory
		if err != nil {
			return err
		}
		f = f2
	case err != nil:
		return err
	}
	defer func() { _ = f.Close() }()

	fl := Flock{f}
	err = fl.Lock()
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()

	if len(mirrors) == 0 {
		for id := range c.Mirrors {
			mirrors = append(mirrors, id)
		}
	}

	well.Go(func(ctx context.Context) error {
		err := updateMirrors(ctx, c, mirrors)
		if err != nil {
			if gcErr := gc(ctx, c); gcErr != nil {
				err = errors.Wrap(err, gcErr.Error())
			}
			return err
		}
		return gc(ctx, c)
	})
	well.Stop()
	return well.Wait()
}
