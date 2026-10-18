package mirror

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybozu-go/aptutil/apt"
)

func makeFileInfo(path string, data []byte) (*apt.FileInfo, error) {
	r := bytes.NewReader(data)
	w := new(bytes.Buffer)
	fi, err := apt.CopyWithFileInfo(w, r, path)
	if err != nil {
		return nil, err
	}
	return fi, nil
}

func testStorageBadConstruction(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp("", "gotest")
	if err != nil {
		t.Fatal(err)
	}
	defer func(f string) {
		if _, err := os.Stat(f); err != nil {
			return
		}
		_ = os.Remove(f)
	}(f.Name())

	_, err = NewStorage(f.Name(), "pre")
	if err == nil {
		t.Error("NewStorage must fail with regular file")
	}

	_ = os.Remove(f.Name())
	_, err = NewStorage(f.Name(), "pre")
	if err == nil {
		t.Error("NewStorage must fail with non-existent directory")
	}
}

func testStorageLookup(t *testing.T) {
	t.Parallel()

	d := t.TempDir()

	s, err := NewStorage(d, "pre")
	if err != nil {
		t.Fatal(err)
	}

	err = s.Load()
	if err != nil {
		t.Error(err)
	}

	files := map[string][]byte{
		"a/b/c":   {'a', 'b', 'c'},
		"def":     {'d', 'e', 'f'},
		"a/pp/le": {'a', 'p', 'p', 'l', 'e'},
	}

	for fn, data := range files {
		tempfile, err := s.TempFile()
		if err != nil {
			t.Fatal(err)
		}

		fi, err := apt.CopyWithFileInfo(tempfile, bytes.NewReader(data), fn)
		if err != nil {
			t.Fatal(err)
		}
		_ = tempfile.Close()

		if err := s.StoreLink(fi, tempfile.Name()); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(tempfile.Name())
	}

	fi, err := makeFileInfo("a/b/c", []byte{'a', 'b', 'd'})
	if err != nil {
		t.Fatal(err)
	}
	fi2, fullpath := s.Lookup(fi, false)
	if fi2 != nil {
		t.Error(`fi2 != nil`)
	}
	if len(fullpath) != 0 {
		t.Error(`len(fullpath) != 0`)
	}

	fi, err = makeFileInfo("a/b/c", files["a/b/c"])
	if err != nil {
		t.Fatal(err)
	}
	fi3, _ := s.Lookup(fi, false)
	if fi3 == nil {
		t.Error(`fi3 == nil`)
	}

	err = s.Save()
	if err != nil {
		t.Fatal(err)
	}

	s2, err := NewStorage(d, "pre")
	if err != nil {
		t.Fatal(err)
	}

	err = s2.Load()
	if err != nil {
		t.Error(err)
	}

	fi, err = makeFileInfo("a/b/c", files["a/b/c"])
	if err != nil {
		t.Fatal(err)
	}
	fi4, _ := s2.Lookup(fi, false)
	if fi4 == nil {
		t.Error(`fi4 == nil`)
	}

	fi, err = makeFileInfo("def", files["def"])
	if err != nil {
		t.Fatal(err)
	}
	fi5, _ := s2.Lookup(fi, false)
	if fi5 == nil {
		t.Error(`fi5 == nil`)
	}

	fi, err = makeFileInfo("a/pp/le", files["a/pp/le"])
	if err != nil {
		t.Fatal(err)
	}
	fi6, _ := s2.Lookup(fi, false)
	if fi6 == nil {
		t.Error(`fi6 == nil`)
	}

	fi, err = makeFileInfo("a/pp/le", files["def"])
	if err != nil {
		t.Fatal(err)
	}
	fi7, _ := s2.Lookup(fi, false)
	if fi7 != nil {
		t.Error(`fi7 != nil`)
	}

	// files recorded in info.json but missing or truncated on disk
	// are not found so that they are downloaded again.
	if err := os.Remove(filepath.Join(d, "pre", "def")); err != nil {
		t.Fatal(err)
	}
	fi, err = makeFileInfo("def", files["def"])
	if err != nil {
		t.Fatal(err)
	}
	if fi8, _ := s2.Lookup(fi, false); fi8 != nil {
		t.Error(`a missing file must not be found`)
	}
	if err := os.Truncate(filepath.Join(d, "pre", "a", "b", "c"), 1); err != nil {
		t.Fatal(err)
	}
	fi, err = makeFileInfo("a/b/c", files["a/b/c"])
	if err != nil {
		t.Fatal(err)
	}
	if fi9, _ := s2.Lookup(fi, false); fi9 != nil {
		t.Error(`a truncated file must not be found`)
	}
}

func testStorageStore(t *testing.T) {
	t.Parallel()

	d := t.TempDir()

	s, err := NewStorage(d, "pre")
	if err != nil {
		t.Fatal(err)
	}

	err = s.Load()
	if err != nil {
		t.Error(err)
	}

	tempfile, err := s.TempFile()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := apt.CopyWithFileInfo(tempfile, strings.NewReader("abc"), "a/b/c")
	_ = tempfile.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = s.StoreLink(fi, tempfile.Name())
	if err != nil {
		t.Error(err)
	}
	found, _ := s.Lookup(fi, false)
	if found == nil {
		t.Error(`found == nil`)
	}

	// duplicates should not be granted
	err = s.StoreLink(fi, tempfile.Name())
	_ = os.Remove(tempfile.Name())
	if err == nil {
		t.Error(`err == nil`)
	}

	tempfile, err = s.TempFile()
	if err != nil {
		t.Fatal(err)
	}
	fi, err = apt.CopyWithFileInfo(tempfile, strings.NewReader("def"), "a/b/c")
	if err != nil {
		t.Fatal(err)
	}
	_ = tempfile.Close()
	err = s.StoreLinkWithHash(fi, tempfile.Name())
	_ = os.Remove(tempfile.Name())
	if err != nil {
		t.Error(err)
	}
	notfound, _ := s.Lookup(fi, false)
	if notfound != nil {
		t.Error(`notfound != nil`)
	}
	found, _ = s.Lookup(fi, true)
	if found == nil {
		t.Error(`found == nil`, d, fi.SHA256Path())
	}
}

func TestStorage(t *testing.T) {
	t.Run("BadConstruction", testStorageBadConstruction)
	t.Run("Lookup", testStorageLookup)
	t.Run("Store", testStorageStore)
}

// TestStorageLookupWithoutSHA512 tests that a file recorded without
// SHA512 by older versions is reused if it matches SHA512 listed now.
func TestStorageLookupWithoutSHA512(t *testing.T) {
	t.Parallel()

	const p = "pool/a.deb"
	data := []byte("data")
	fi, err := makeFileInfo(p, data)
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		name   string
		onDisk []byte
		found  bool
	}{
		{name: "same", onDisk: data, found: true},
		{name: "corrupted", onDisk: []byte("atad")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, err := NewStorage(t.TempDir(), "pre")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()

			// info.json written by older versions has no SHA512Sum.
			old := new(apt.FileInfo)
			j := fmt.Sprintf(`{"Path":%q,"Size":%d,"SHA256Sum":%q}`, p, len(data), path.Base(fi.SHA256Path()))
			if err := json.Unmarshal([]byte(j), old); err != nil {
				t.Fatal(err)
			}
			s.info[p] = old
			fp := filepath.Join(s.Dir(), "pre", p)
			if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fp, tc.onDisk, 0o644); err != nil {
				t.Fatal(err)
			}

			fi2, fullpath := s.Lookup(fi, true)
			if !tc.found {
				if fi2 != nil {
					t.Error("a corrupted file must not be found")
				}
				return
			}
			if fi2 == nil || fullpath != fp {
				t.Fatalf("Lookup = %v, %q, want the file", fi2, fullpath)
			}
			if fi2.SHA512Path() != fi.SHA512Path() {
				t.Errorf("SHA512Path() = %q, want %q", fi2.SHA512Path(), fi.SHA512Path())
			}
		})
	}
}
