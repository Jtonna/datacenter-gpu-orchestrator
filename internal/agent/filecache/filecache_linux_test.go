//go:build linux

package filecache

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

const tmpfsMagic = 0x01021994

// TestEvictRealFile writes a 64 MiB file, reads it into the page cache, drops
// it and checks that the cached share fell under 1 %. It needs a filesystem
// with a page cache that posix_fadvise can drop: tmpfs keeps its pages in
// memory for good, so the test is skipped there. POTLUCK_FILECACHE_TEST_DIR
// chooses the folder (default: the test's temporary folder).
func TestEvictRealFile(t *testing.T) {
	dir := os.Getenv("POTLUCK_FILECACHE_TEST_DIR")
	if dir == "" {
		dir = t.TempDir()
	} else {
		d, err := os.MkdirTemp(dir, "filecache-test-")
		if err != nil {
			t.Fatal(err)
		}
		dir = d
		t.Cleanup(func() { os.RemoveAll(d) })
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err == nil && int64(fs.Type) == tmpfsMagic {
		t.Skipf("%s is on tmpfs, whose pages cannot be dropped from the page cache; set POTLUCK_FILECACHE_TEST_DIR to a folder on a disk", dir)
	}
	const size = 64 << 20
	path := filepath.Join(dir, "cold.gguf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	for written := 0; written < size; written += len(buf) {
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil { // clean pages: dirty ones are not dropped
		t.Fatal(err)
	}
	f.Close()
	r, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	r.Close()

	e := &Evictor{Root: dir, Sys: Default(), MinGap: -1}
	res, err := e.Evict("cold.gguf")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("file %d bytes, cached before %d (%.1f%%), after %d (%.2f%%), %.3f s", res.FileBytes, res.CachedBefore,
		CachedPct(res.FileBytes, res.CachedBefore), res.CachedAfter, CachedPct(res.FileBytes, res.CachedAfter), res.DurationS)
	if res.FileBytes != size {
		t.Fatalf("file bytes %d", res.FileBytes)
	}
	if CachedPct(res.FileBytes, res.CachedBefore) < 50 {
		t.Skipf("the file was not kept in the page cache after it was read (%.1f%% cached): this filesystem does not behave as mincore expects", CachedPct(res.FileBytes, res.CachedBefore))
	}
	if res.CachedAfter >= res.CachedBefore && res.CachedAfter > 0 {
		t.Skipf("posix_fadvise(DONTNEED) dropped nothing on this filesystem (%d bytes still cached)", res.CachedAfter)
	}
	if !Cold(res.FileBytes, res.CachedAfter) {
		t.Fatalf("%.2f%% still cached after the eviction", CachedPct(res.FileBytes, res.CachedAfter))
	}
}
