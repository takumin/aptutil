package cacher

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cybozu-go/aptutil/apt"
)

func newTestStorage(t *testing.T, dir string, capacity uint64) *Storage {
	t.Helper()

	cm, err := NewStorage(dir, capacity)
	if err != nil {
		t.Fatal(err)
	}
	return cm
}

func insert(cm *Storage, data []byte, path string) (*apt.FileInfo, error) {
	f, err := cm.TempFile()
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()

	fi, err := apt.CopyWithFileInfo(f, bytes.NewReader(data), path)
	if err != nil {
		return nil, err
	}

	err = f.Sync()
	if err != nil {
		return nil, err
	}

	err = cm.Insert(f.Name(), fi)
	return fi, err
}

func testStorageInsertWorksCorrectly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cm := newTestStorage(t, dir, 0)

	fi, err := insert(cm, []byte("a"), "path/to/a")
	if err != nil {
		t.Fatal(err)
	}

	if cm.Len() != 1 {
		t.Error(`cm.Len() != 1`)
	}

	_, err = cm.Lookup(fi)
	if err != nil {
		t.Error(`cannot lookup inserted file`)
	}
}

func testStorageInsertOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cm := newTestStorage(t, dir, 0)

	_, err := insert(cm, []byte("a"), "path/to/a")
	if err != nil {
		t.Fatal(err)
	}

	fi, err := insert(cm, []byte("a"), "path/to/a")
	if err != nil {
		t.Fatal(err)
	}

	if cm.Len() != 1 {
		t.Error(`cm.Len() != 1`)
	}

	_, err = cm.Lookup(fi)
	if err != nil {
		t.Error(`cannot lookup inserted file`)
	}
}

func testStorageInsertReturnsErrorAgainstBadPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cm := newTestStorage(t, dir, 0)

	cases := []struct{ Title, Path string }{
		{
			Title: "Absolute path",
			Path:  "/absolute/path",
		},
		{
			Title: "Uncleaned path",
			Path:  "./uncleaned/path",
		},
		{
			Title: "Empty path",
			Path:  "",
		},
		{
			Title: ".",
			Path:  ".",
		},
		{
			Title: "Parent traversal",
			Path:  "../foo",
		},
		{
			Title: "Nested parent traversal",
			Path:  "a/../../b",
		},
	}

	for _, tc := range cases {
		t.Run(tc.Title, func(t *testing.T) {
			_, err := insert(cm, []byte("a"), tc.Path)
			if !errors.Is(err, ErrBadPath) {
				t.Fatal(err)
			}
		})
	}
}

func testStorageInsertPurgesFilesAllowingLRU(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cm := newTestStorage(t, dir, 3)

	fiA, err := insert(cm, []byte("a"), "a")
	if err != nil {
		t.Fatal(err)
	}

	fiBC, err := insert(cm, []byte("bc"), "bc")
	if err != nil {
		t.Fatal(err)
	}

	// a and bc will be purged
	fiDE, err := insert(cm, []byte("de"), "de")
	if err != nil {
		t.Fatal(err)
	}

	if cm.Len() != 1 {
		t.Error(`cmd.Len() != 1`)
	}
	_, err = cm.Lookup(fiA)
	if !errors.Is(err, ErrNotFound) {
		t.Error(`err != ErrNotFound`)
	}
	_, err = cm.Lookup(fiBC)
	if !errors.Is(err, ErrNotFound) {
		t.Error(`err != ErrNotFound`)
	}

	fiA, err = insert(cm, []byte("a"), "a")
	if err != nil {
		t.Fatal(err)
	}

	// touch de
	_, err = cm.Lookup(fiDE)
	if err != nil {
		t.Error(err)
	}

	// a will be purged
	fiF, err := insert(cm, []byte("f"), "f")
	if err != nil {
		t.Fatal(err)
	}

	_, err = cm.Lookup(fiA)
	if !errors.Is(err, ErrNotFound) {
		t.Error(`err != ErrNotFound`)
	}
	_, err = cm.Lookup(fiDE)
	if err != nil {
		t.Error(err)
	}
	_, err = cm.Lookup(fiF)
	if err != nil {
		t.Error(err)
	}
}

func testStorageInsertRejectsTooLarge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cm := newTestStorage(t, dir, 3)

	fiA, err := insert(cm, []byte("ab"), "a")
	if err != nil {
		t.Fatal(err)
	}

	// an item larger than the capacity would be evicted immediately
	// after evicting all other items.
	_, err = insert(cm, []byte("abcd"), "b")
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}

	f, err := cm.Lookup(fiA)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if cm.Len() != 1 {
		t.Errorf("cm.Len() = %d, want 1", cm.Len())
	}
}

func TestStorageInsert(t *testing.T) {
	t.Run("Storage.Insert should insert file", testStorageInsertWorksCorrectly)
	t.Run("Storage.Insert should overwrite", testStorageInsertOverwrite)
	t.Run("Storage.Insert should return error if passed FileInfo path is bad path", testStorageInsertReturnsErrorAgainstBadPath)
	t.Run("Storage.Insert should purge files allowing LRU", testStorageInsertPurgesFilesAllowingLRU)
	t.Run("Storage.Insert should reject items larger than capacity", testStorageInsertRejectsTooLarge)
}

func TestNewStorage(t *testing.T) {
	t.Parallel()

	if _, err := NewStorage("relative", 0); err == nil {
		t.Error("NewStorage must fail with a relative path")
	}

	dir := filepath.Join(t.TempDir(), "a", "b")
	if _, err := NewStorage(dir, 0); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Errorf("NewStorage must create %s", dir)
	}

	// failures to create the directory must be reported as errors.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStorage(filepath.Join(f, "sub"), 0); err == nil {
		t.Error("NewStorage must fail under a regular file")
	}
	if os.Geteuid() != 0 {
		ro := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(ro, 0o555); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStorage(filepath.Join(ro, "sub"), 0); err == nil {
			t.Error("NewStorage must fail in a read-only directory")
		}
	}
}

func makeFileInfo(path string, data []byte) (*apt.FileInfo, error) {
	rb := bytes.NewReader(data)
	wb := new(bytes.Buffer)
	fi, err := apt.CopyWithFileInfo(wb, rb, path)
	if err != nil {
		return nil, err
	}
	return fi, nil
}

func TestStorageLoad(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{
		"a":    {'a'},
		"bc":   {'b', 'c'},
		"def":  {'d', 'e', 'f'},
		"ghij": {'g', 'h', 'i', 'j'},
	}

	dir := t.TempDir()

	for k, v := range files {
		err := os.WriteFile(filepath.Join(dir, k+fileSuffix), v, 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}

	// dummy should be ignored as it does not have a proper suffix.
	err := os.WriteFile(filepath.Join(dir, "dummy"), []byte{'d'}, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	cm := newTestStorage(t, dir, 0)
	err = cm.Load()
	if err != nil {
		t.Fatal(err)
	}

	l := cm.ListAll()
	if len(l) != len(files) {
		t.Error(`len(l) != len(files)`)
	}

	fiA, err := makeFileInfo("a", files["a"])
	if err != nil {
		t.Error(err)
	}
	fA, err := cm.Lookup(fiA)
	if err != nil {
		t.Error(err)
	}
	_ = fA.Close()
	fiBC, err := makeFileInfo("bc", files["bc"])
	if err != nil {
		t.Error(err)
	}
	fBC, err := cm.Lookup(fiBC)
	if err != nil {
		t.Error(err)
	}
	_ = fBC.Close()
	fiDEF, err := makeFileInfo("def", files["def"])
	if err != nil {
		t.Error(err)
	}
	fDEF, err := cm.Lookup(fiDEF)
	if err != nil {
		t.Error(err)
	}
	_ = fDEF.Close()

	fiGHIJ, err := makeFileInfo("ghij", files["ghij"])
	if err != nil {
		t.Error(err)
	}
	fGHIJ, err := cm.Lookup(fiGHIJ)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(fGHIJ)
	_ = fGHIJ.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(files["ghij"], data) {
		t.Error(`!bytes.Equal(files["ghij"], data)`)
	}
}

func TestStorageLoadRemovesTempFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cm := newTestStorage(t, dir, 0)

	// a temporary file left by an interrupted download.
	f, err := cm.TempFile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// items whose names begin with the prefix are not temporary files.
	items := []string{"_tmpitem", filepath.Join("ubuntu", "_tmpitem")}
	for _, p := range items {
		fp := filepath.Join(dir, p+fileSuffix)
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte{'a'}, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := cm.Load(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(f.Name()); !os.IsNotExist(err) {
		t.Errorf("temporary file is not removed: %v", err)
	}
	if l := cm.ListAll(); len(l) != len(items) {
		t.Errorf("len(ListAll()) = %d, want %d", len(l), len(items))
	}
	if cm.used != uint64(len(items)) {
		t.Errorf("used = %d, want %d", cm.used, len(items))
	}
}

func TestStorageLookupCalculatesChecksums(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data := bytes.Repeat([]byte("data"), 1<<16)
	err := os.WriteFile(filepath.Join(dir, "a"+fileSuffix), data, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	cm := newTestStorage(t, dir, 0)
	if err := cm.Load(); err != nil {
		t.Fatal(err)
	}

	fi, err := makeFileInfo("a", data)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := makeFileInfo("a", bytes.Repeat([]byte("atad"), 1<<16))
	if err != nil {
		t.Fatal(err)
	}

	// look up concurrently while checksums are not calculated yet.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			f, err := cm.Lookup(fi)
			if err != nil {
				t.Error(err)
				return
			}
			_ = f.Close()
		})
		wg.Go(func() {
			if _, err := cm.Lookup(bad); !errors.Is(err, ErrNotFound) {
				t.Errorf("Lookup(bad) = %v, want ErrNotFound", err)
			}
		})
	}
	wg.Wait()

	cm.mu.Lock()
	defer cm.mu.Unlock()
	if !cm.cache["a"].HasChecksum() {
		t.Error("checksums should be calculated")
	}
	if cm.used != uint64(len(data)) {
		t.Errorf("used = %d, want %d", cm.used, len(data))
	}
}
