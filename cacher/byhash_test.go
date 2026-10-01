package cacher

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cybozu-go/aptutil/apt"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func gzipData(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const (
	byHashRelease  = "dists/noble/Release"
	byHashPackages = "dists/noble/main/binary-amd64/Packages.gz"
	byHashDeb      = "pool/a.deb"
	byHashDebData  = "deb data"
)

// setupByHashRepo serves a repository whose Packages.gz is listed in
// Release with Acquire-By-Hash.  It returns the by-hash path of
// Packages.gz.
func setupByHashRepo(t *testing.T, tr *testRepo) string {
	t.Helper()

	packages := gzipData(t, []byte(fmt.Sprintf(
		"Package: a\nFilename: %s\nSize: %d\nSHA256: %s\n",
		byHashDeb, len(byHashDebData), sha256Hex([]byte(byHashDebData)))))
	release := fmt.Sprintf("Acquire-By-Hash: yes\nSHA256:\n %s %d main/binary-amd64/Packages.gz\n",
		sha256Hex(packages), len(packages))
	byHashPath := "dists/noble/main/binary-amd64/by-hash/SHA256/" + sha256Hex(packages)

	tr.set(byHashRelease, []byte(release))
	tr.set(byHashPath, packages)
	tr.set(byHashDeb, []byte(byHashDebData))
	return byHashPath
}

func TestCacherGetByHash(t *testing.T) {
	t.Parallel()

	tr := newTestRepo()
	byHashPath := setupByHashRepo(t, tr)
	srv := httptest.NewServer(tr)
	defer srv.Close()

	c := newTestCacher(t, srv.URL)

	if status, _ := getData(t, c, "ubuntu/"+byHashRelease); status != http.StatusOK {
		t.Fatalf("status of Release = %d", status)
	}

	// Packages.gz retrieved via by-hash is cached and parsed as such.
	if status, _ := getData(t, c, "ubuntu/"+byHashPath); status != http.StatusOK {
		t.Fatalf("status of by-hash = %d", status)
	}
	if status, _ := getData(t, c, "ubuntu/"+byHashPackages); status != http.StatusOK {
		t.Fatalf("status of Packages.gz = %d", status)
	}
	if n := tr.hits(byHashPackages); n != 0 {
		t.Errorf("Packages.gz was downloaded %d times, want 0", n)
	}
	c.fiLock.RLock()
	_, listed := c.info["ubuntu/"+byHashDeb]
	c.fiLock.RUnlock()
	if !listed {
		t.Fatal("items in Packages.gz retrieved via by-hash are not registered")
	}

	// the item listed in Packages.gz is now validated.
	tr.set(byHashDeb, []byte("broken"))
	if status, _ := getData(t, c, "ubuntu/"+byHashDeb); status != http.StatusBadGateway {
		t.Errorf("status of a broken item = %d, want %d", status, http.StatusBadGateway)
	}

	// Packages.gz is recovered from meta_dir after restart.
	c2, err := NewCacher(&Config{
		CheckInterval:  defaultCheckInterval,
		CachePeriod:    defaultCachePeriod,
		MetaDirectory:  c.meta.dir,
		CacheDirectory: c.items.dir,
		CacheCapacity:  defaultCacheCapacity,
		Mapping:        map[string]string{"ubuntu": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.aliases["ubuntu/"+byHashPath]; !ok {
		t.Error("by-hash paths are not recovered")
	}
	if _, ok := c2.info["ubuntu/"+byHashDeb]; !ok {
		t.Error("items in Packages.gz are not recovered")
	}
}

func TestCacherGetByHashInvalid(t *testing.T) {
	t.Parallel()

	tr := newTestRepo()
	byHashPath := setupByHashRepo(t, tr)
	tr.set(byHashPath, []byte("broken"))
	srv := httptest.NewServer(tr)
	defer srv.Close()

	c := newTestCacher(t, srv.URL)

	if status, _ := getData(t, c, "ubuntu/"+byHashRelease); status != http.StatusOK {
		t.Fatalf("status of Release = %d", status)
	}
	if status, _ := getData(t, c, "ubuntu/"+byHashPath); status != http.StatusBadGateway {
		t.Errorf("status of a broken by-hash = %d, want %d", status, http.StatusBadGateway)
	}
}

func TestCacherUpdateListedAliases(t *testing.T) {
	t.Parallel()

	c := newTestCacher(t, "http://example.com")

	const (
		release = "ubuntu/dists/noble/Release"
		gz      = "ubuntu/dists/noble/main/binary-amd64/Packages.gz"
	)
	oldFI, err := makeFileInfo(gz, []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	newFI, err := makeFileInfo(gz, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}

	c.updateListed(release, []*apt.FileInfo{oldFI}, byHashAliases([]*apt.FileInfo{oldFI}))
	if fi := c.aliases[oldFI.SHA256Path()]; fi != oldFI {
		t.Fatalf("aliases[%s] = %v, want %v", oldFI.SHA256Path(), fi, oldFI)
	}

	c.updateListed(release, []*apt.FileInfo{newFI}, byHashAliases([]*apt.FileInfo{newFI}))
	for _, hp := range []string{oldFI.SHA256Path(), oldFI.SHA1Path(), oldFI.MD5SumPath()} {
		if _, ok := c.aliases[hp]; ok {
			t.Errorf("%s should be removed", hp)
		}
	}
	if fi := c.aliases[newFI.SHA256Path()]; fi != newFI {
		t.Errorf("aliases[%s] = %v, want %v", newFI.SHA256Path(), fi, newFI)
	}
}
