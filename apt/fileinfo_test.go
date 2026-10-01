package apt

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func testFileInfoSame(t *testing.T) {
	t.Parallel()

	data := []byte{'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i'}
	md5sum := md5.Sum(data)
	sha1sum := sha1.Sum(data)
	sha256sum := sha256.Sum256(data)

	data2 := []byte{'1', '2', '3'}
	md5sum2 := md5.Sum(data2)
	sha1sum2 := sha1.Sum(data2)

	fi := &FileInfo{
		path:      "/data",
		size:      uint64(len(data)),
		md5sum:    md5sum[:],
		sha1sum:   sha1sum[:],
		sha256sum: sha256sum[:],
	}

	if fi.Path() != "/data" {
		t.Error(`fi.Path() != "/data"`)
	}

	badpath := &FileInfo{
		path: "bad",
		size: uint64(len(data)),
	}
	if badpath.Same(fi) {
		t.Error(`badpath.Same(fi)`)
	}

	pathonly := &FileInfo{
		path: "/data",
		size: uint64(len(data)),
	}
	if !pathonly.Same(fi) {
		t.Error(`!pathonly.Same(fi)`)
	}

	sizemismatch := &FileInfo{
		path: "/data",
		size: 0,
	}
	if sizemismatch.Same(fi) {
		t.Error(`sizemismatch.Same(fi)`)
	}

	md5mismatch := &FileInfo{
		path:   "/data",
		size:   uint64(len(data)),
		md5sum: md5sum2[:],
	}
	if md5mismatch.Same(fi) {
		t.Error(`md5mismatch.Same(fi)`)
	}

	md5match := &FileInfo{
		path:   "/data",
		size:   uint64(len(data)),
		md5sum: md5sum[:],
	}
	if !md5match.Same(fi) {
		t.Error(`!md5match.Same(fi)`)
	}

	sha1mismatch := &FileInfo{
		path:    "/data",
		size:    uint64(len(data)),
		md5sum:  md5sum[:],
		sha1sum: sha1sum2[:],
	}
	if sha1mismatch.Same(fi) {
		t.Error(`sha1mismatch.Same(fi)`)
	}

	sha1match := &FileInfo{
		path:    "/data",
		size:    uint64(len(data)),
		md5sum:  md5sum[:],
		sha1sum: sha1sum[:],
	}
	if !sha1match.Same(fi) {
		t.Error(`!sha1match.Same(fi)`)
	}

	sha1matchmd5mismatch := &FileInfo{
		path:    "/data",
		size:    uint64(len(data)),
		md5sum:  md5sum2[:],
		sha1sum: sha1sum[:],
	}
	if sha1matchmd5mismatch.Same(fi) {
		t.Error(`sha1matchmd5mismatch.Same(fi)`)
	}

	allmatch := &FileInfo{
		path:      "/data",
		size:      uint64(len(data)),
		md5sum:    md5sum[:],
		sha1sum:   sha1sum[:],
		sha256sum: sha256sum[:],
	}
	if !allmatch.Same(fi) {
		t.Error(`!allmatch.Same(fi)`)
	}
}

func testFileInfoConflicts(t *testing.T) {
	t.Parallel()

	data := []byte("abc")
	md5sum := md5.Sum(data)
	sha256sum := sha256.Sum256(data)
	sha256sum2 := sha256.Sum256([]byte("xyz"))

	fi := &FileInfo{
		path:      "/data",
		size:      uint64(len(data)),
		md5sum:    md5sum[:],
		sha256sum: sha256sum[:],
	}

	testCases := []struct {
		name string
		t    *FileInfo
		want bool
	}{
		{"same", fi, false},
		{"sha256 only", &FileInfo{path: "/data", size: 3, sha256sum: sha256sum[:]}, false},
		{"no common checksums", &FileInfo{path: "/data", size: 3, sha1sum: []byte("x")}, false},
		{"path", &FileInfo{path: "/other", size: 3}, true},
		{"size", &FileInfo{path: "/data", size: 4}, true},
		{"sha256", &FileInfo{path: "/data", size: 3, sha256sum: sha256sum2[:]}, true},
	}
	for _, tc := range testCases {
		if got := fi.Conflicts(tc.t); got != tc.want {
			t.Errorf("%s: fi.Conflicts(t) = %v, want %v", tc.name, got, tc.want)
		}
		if got := tc.t.Conflicts(fi); got != tc.want {
			t.Errorf("%s: t.Conflicts(fi) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func testFileInfoJSON(t *testing.T) {
	t.Parallel()

	r := strings.NewReader("hello world")
	w := new(bytes.Buffer)
	p := "/abc/def"

	fi, err := CopyWithFileInfo(w, r, p)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(fi)
	if err != nil {
		t.Fatal(err)
	}

	fi2 := new(FileInfo)
	err = json.Unmarshal(j, fi2)
	if err != nil {
		t.Fatal(err)
	}

	if !fi.Same(fi2) {
		t.Error(`!fi.Same(fi2)`)
		t.Logf("%#v", fi2)
	}
}

func testFileInfoJSONMissingChecksums(t *testing.T) {
	t.Parallel()

	// Release files of Debian and Ubuntu have no SHA1 field.
	fi := &FileInfo{
		path:      "dists/s/main/binary-amd64/Packages",
		size:      3,
		md5sum:    []byte{0x01},
		sha256sum: []byte{0x02},
	}
	j, err := json.Marshal(fi)
	if err != nil {
		t.Fatal(err)
	}

	fi2 := new(FileInfo)
	err = json.Unmarshal(j, fi2)
	if err != nil {
		t.Fatal(err)
	}

	if fi2.sha1sum != nil {
		t.Errorf("sha1sum = %#v, want nil", fi2.sha1sum)
	}
	if p := fi2.SHA1Path(); p != "" {
		t.Errorf("SHA1Path() = %q, want empty", p)
	}
	if !fi.Same(fi2) || !fi2.Same(fi) {
		t.Error("FileInfo changed by JSON round trip")
	}

	fi3 := new(FileInfo)
	err = json.Unmarshal([]byte(`{"Path":"a","Size":0}`), fi3)
	if err != nil {
		t.Fatal(err)
	}
	if fi3.HasChecksum() {
		t.Error("FileInfo without checksums must not have checksums")
	}
}

func testFileInfoAddPrefix(t *testing.T) {
	t.Parallel()

	r := strings.NewReader("hello world")
	w := new(bytes.Buffer)
	p := "/abc/def"

	fi, err := CopyWithFileInfo(w, r, p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Path() != "/abc/def" {
		t.Error(`fi.Path() != "/abc/def"`)
	}

	fi = fi.AddPrefix("/prefix")
	if fi.Path() != "/prefix/abc/def" {
		t.Error(`fi.Path() != "/prefix/abc/def"`)
	}
}

func testFileInfoChecksum(t *testing.T) {
	t.Parallel()

	text := "hello world"
	r := strings.NewReader(text)
	w := new(bytes.Buffer)
	p := "/abc/def"

	md5sum := md5.Sum([]byte(text))
	sha1sum := sha1.Sum([]byte(text))
	sha256sum := sha256.Sum256([]byte(text))
	m5 := hex.EncodeToString(md5sum[:])
	s1 := hex.EncodeToString(sha1sum[:])
	s256 := hex.EncodeToString(sha256sum[:])

	fi, err := CopyWithFileInfo(w, r, p)
	if err != nil {
		t.Fatal(err)
	}

	if fi.MD5SumPath() != "/abc/by-hash/MD5Sum/"+m5 {
		t.Error(`fi.MD5SumPath() != "/abc/by-hash/MD5Sum/" + md5`)
	}
	if fi.SHA1Path() != "/abc/by-hash/SHA1/"+s1 {
		t.Error(`fi.SHA1Path() != "/abc/by-hash/SHA1/" + s1`)
	}
	if fi.SHA256Path() != "/abc/by-hash/SHA256/"+s256 {
		t.Error(`fi.SHA256Path() != "/abc/by-hash/SHA256/" + s256`)
	}
}

func testFileInfoCopy(t *testing.T) {
	t.Parallel()

	text := "hello world"
	r := strings.NewReader(text)
	w := new(bytes.Buffer)
	p := "/abc/def"

	md5sum := md5.Sum([]byte(text))
	sha1sum := sha1.Sum([]byte(text))
	sha256sum := sha256.Sum256([]byte(text))

	fi := &FileInfo{
		path:      p,
		size:      uint64(r.Size()), //nolint:gosec // G115: Size of a strings.Reader is non-negative
		md5sum:    md5sum[:],
		sha1sum:   sha1sum[:],
		sha256sum: sha256sum[:],
	}

	fi2, err := CopyWithFileInfo(w, r, p)
	if err != nil {
		t.Fatal(err)
	}
	if w.String() != text {
		t.Errorf(
			"Copy did not work properly, expected: %s, actual: %s",
			text, w.String(),
		)
	}
	if !fi.Same(fi2) {
		t.Error("Generated FileInfo is invalid")
	}
}

func testFileInfoCopyLarge(t *testing.T) {
	t.Parallel()

	// larger than the buffer of io.Copy to be written in chunks.
	data := make([]byte, 1<<20+1)
	for i := range data {
		data[i] = byte(i * 7)
	}
	md5sum := md5.Sum(data)
	sha1sum := sha1.Sum(data)
	sha256sum := sha256.Sum256(data)

	w := new(bytes.Buffer)
	fi, err := CopyWithFileInfo(w, bytes.NewReader(data), "/large")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Bytes(), data) {
		t.Error("Copy did not work properly")
	}
	if !bytes.Equal(fi.md5sum, md5sum[:]) ||
		!bytes.Equal(fi.sha1sum, sha1sum[:]) ||
		!bytes.Equal(fi.sha256sum, sha256sum[:]) {
		t.Error("Generated FileInfo is invalid")
	}
}

func BenchmarkCopyWithFileInfo(b *testing.B) {
	data := make([]byte, 8<<20)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, err := CopyWithFileInfo(io.Discard, bytes.NewReader(data), "/bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func TestFileInfo(t *testing.T) {
	t.Run("Same", testFileInfoSame)
	t.Run("Conflicts", testFileInfoConflicts)
	t.Run("JSON", testFileInfoJSON)
	t.Run("JSONMissingChecksums", testFileInfoJSONMissingChecksums)
	t.Run("AddPrefix", testFileInfoAddPrefix)
	t.Run("Checksum", testFileInfoChecksum)
	t.Run("Copy", testFileInfoCopy)
	t.Run("CopyLarge", testFileInfoCopyLarge)
}
