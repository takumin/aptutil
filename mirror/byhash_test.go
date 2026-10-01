package mirror

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cybozu-go/aptutil/apt"
)

const testIndexPath = "dists/s/main/binary-amd64/Packages"

// releaseFileInfo returns FileInfo of testIndexPath listed in a Release
// file that has only the given checksum field.
func releaseFileInfo(t *testing.T, field string, data []byte) *apt.FileInfo {
	t.Helper()

	full, err := apt.CopyWithFileInfo(io.Discard, bytes.NewReader(data), testIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	// by-hash paths end with the hex-encoded checksum.
	var csum string
	switch field {
	case "MD5Sum":
		csum = path.Base(full.MD5SumPath())
	case "SHA256":
		csum = path.Base(full.SHA256Path())
	default:
		t.Fatalf("unknown field: %s", field)
	}
	release := fmt.Sprintf("Acquire-By-Hash: yes\n%s:\n %s %d main/binary-amd64/Packages\n",
		field, csum, len(data))

	fil, _, err := apt.ExtractFileInfo("dists/s/Release", strings.NewReader(release))
	if err != nil {
		t.Fatal(err)
	}
	if len(fil) != 1 {
		t.Fatalf("len(fil) = %d, want 1", len(fil))
	}
	return fil[0]
}

func TestStorageStoreLinkWithHashPartialChecksums(t *testing.T) {
	t.Parallel()

	data := []byte("foo")
	fi := releaseFileInfo(t, "SHA256", data)

	s, err := NewStorage(t.TempDir(), "pre")
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(s.Dir(), "tmp")
	if err := os.WriteFile(f, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreLinkWithHash(fi, f); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.info[""]; ok {
		t.Error(`an entry for "" must not be stored`)
	}
	for _, p := range []string{fi.Path(), fi.SHA256Path()} {
		if _, ok := s.info[p]; !ok {
			t.Errorf("no entry for %s", p)
		}
		if _, err := os.Stat(filepath.Join(s.Dir(), "pre", p)); err != nil {
			t.Error(err)
		}
	}
}

func TestMirrorDownloadPartialChecksums(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		// never match the checksum to try all by-hash paths.
		_, _ = w.Write([]byte("bar"))
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	fi := releaseFileInfo(t, "SHA256", []byte("foo"))
	ch := make(chan *dlResult, 1)
	m.download(context.Background(), testIndexPath, fi, true, ch)
	r := <-ch
	if r.tempfile != nil {
		closeAndRemoveFile(r.tempfile)
	}
	if r.err == nil {
		t.Error("download must fail with invalid checksum")
	}

	want := []string{"/" + testIndexPath, "/" + fi.SHA256Path()}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Errorf("requested paths = %v, want %v", paths, want)
	}
}

func TestMirrorExtractItemsByHashPartialChecksums(t *testing.T) {
	t.Parallel()

	index := []byte("Package: a\nFilename: pool/a.deb\nSize: 1\n")
	fi := releaseFileInfo(t, "MD5Sum", index)

	m := newTestMirror(t, "http://example.com")
	m.mc.Suites = []string{"s"}
	m.mc.Sections = []string{"main"}
	m.mc.Architectures = []string{"amd64"}

	f := filepath.Join(m.storage.Dir(), "tmp")
	if err := os.WriteFile(f, index, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.storage.StoreLinkWithHash(fi, f); err != nil {
		t.Fatal(err)
	}

	itemMap := make(map[string]*apt.FileInfo)
	err := m.extractItems([]*apt.FileInfo{fi}, map[string][]*apt.FileInfo{}, itemMap, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := itemMap["pool/a.deb"]; !ok {
		t.Errorf("itemMap = %v, want pool/a.deb", itemMap)
	}
}
