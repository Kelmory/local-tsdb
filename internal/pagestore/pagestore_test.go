package pagestore

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

func openNew(t *testing.T, path string) *PageStore {
	t.Helper()
	ps, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestFreshHeader(t *testing.T) {
	ps := openNew(t, t.TempDir()+"/db.ltsdb")
	defer ps.Close(false)
	if ps.Header.TotalPages != 1 {
		t.Fatalf("total_pages=%d", ps.Header.TotalPages)
	}
	if ps.Header.FreeListHead != FreeNone {
		t.Fatalf("free_list_head=%d", ps.Header.FreeListHead)
	}
	if ps.Header.RootIndexPage != FreeNone {
		t.Fatalf("root_index_page=%d", ps.Header.RootIndexPage)
	}
}

func TestHeaderCRCCorruptionDetected(t *testing.T) {
	path := t.TempDir() + "/db.ltsdb"
	ps := openNew(t, path)
	ps.Close(false)
	// Flip a byte inside the reserved area (offset 8..15) to break the CRC.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xFF}, 9); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("expected corruption error")
	}
}

func TestSmallBlobRoundtripReopen(t *testing.T) {
	path := t.TempDir() + "/db.ltsdb"
	ps := openNew(t, path)
	ps.Begin()
	loc, err := ps.WriteBlob([]byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := ps.ReadBlob(loc)
	if err != nil || !bytes.Equal(got, []byte("hello world")) {
		t.Fatalf("got %q err %v", got, err)
	}
	ps.Close(false)

	ps2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ps2.Close(false)
	got, err = ps2.ReadBlob(loc)
	if err != nil || !bytes.Equal(got, []byte("hello world")) {
		t.Fatalf("after reopen: %q err %v", got, err)
	}
}

func TestMultiPageChaining(t *testing.T) {
	// Larger than one page's capacity (PageCap = 4088) forces chaining.
	path := t.TempDir() + "/db.ltsdb"
	payload := bytes.Repeat([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, 480) // 7680 bytes
	ps := openNew(t, path)
	ps.Begin()
	loc, err := ps.WriteBlob(payload)
	if err != nil {
		t.Fatal(err)
	}
	ps.Commit()
	got, err := ps.ReadBlob(loc)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("roundtrip failed: len=%d err=%v", len(got), err)
	}
	ps.Close(false)

	ps2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ps2.Close(false)
	got, err = ps2.ReadBlob(loc)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("after reopen: len=%d err=%v", len(got), err)
	}
}

func TestEmptyBlobRejected(t *testing.T) {
	ps := openNew(t, t.TempDir()+"/db.ltsdb")
	defer ps.Close(false)
	ps.Begin()
	if _, err := ps.WriteBlob(nil); err == nil {
		t.Fatal("expected error for empty blob")
	}
}

func TestAllocFreeReuse(t *testing.T) {
	ps := openNew(t, t.TempDir()+"/db.ltsdb")
	defer ps.Close(false)
	ps.Begin()
	a, _ := ps.AllocPage()
	b, _ := ps.AllocPage()
	if err := ps.FreePage(a); err != nil {
		t.Fatal(err)
	}
	if err := ps.FreePage(b); err != nil {
		t.Fatal(err)
	}
	ps.Commit()

	ps.Begin()
	c, _ := ps.AllocPage() // should reuse the most recently freed (b)
	d, _ := ps.AllocPage() // then a
	ps.Commit()
	if c != b || d != a {
		t.Fatalf("got c=%d d=%d, want %d %d", c, d, b, a)
	}
}

// keepWALOnDisk closes the store without checkpointing.
func keepWALOnDisk(t *testing.T, ps *PageStore) {
	t.Helper()
	if err := ps.Close(false); err != nil {
		t.Fatal(err)
	}
}

func TestWALReplayRestoresLostPage(t *testing.T) {
	path := t.TempDir() + "/db.ltsdb"
	data := bytes.Repeat([]byte("CRASHDATA"), 10)
	ps := openNew(t, path)
	ps.Begin()
	loc, _ := ps.WriteBlob(data)
	if err := ps.Commit(); err != nil {
		t.Fatal(err)
	}
	keepWALOnDisk(t, ps) // keep the WAL on disk

	// Simulate a crash that lost the data page from the main file.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, PageSize), PageSize); err != nil { // zero page 1
		t.Fatal(err)
	}
	f.Close()

	// Reopen replays the WAL and restores the lost page.
	ps2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ps2.Close(false)
	got, err := ps2.ReadBlob(loc)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("replay failed: len=%d err=%v", len(got), err)
	}
}

func TestTornWALFrameIsIgnored(t *testing.T) {
	path := t.TempDir() + "/db.ltsdb"
	ps := openNew(t, path)
	ps.Begin()
	loc, _ := ps.WriteBlob([]byte("STILL-HERE"))
	ps.Commit()
	keepWALOnDisk(t, ps)

	// Append a truncated, corrupt frame to the WAL.
	wal, err := os.OpenFile(path+".wal", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 16)
	binary.LittleEndian.PutUint64(head[0:], 999)
	binary.LittleEndian.PutUint32(head[8:], 1)
	binary.LittleEndian.PutUint32(head[12:], 0xDEADBEEF)
	wal.Write(head)
	wal.Write(make([]byte, 16)) // far shorter than a full page image
	wal.Close()

	// Reopen must stop at the bad frame and keep prior good data.
	ps2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ps2.Close(false)
	got, err := ps2.ReadBlob(loc)
	if err != nil || !bytes.Equal(got, []byte("STILL-HERE")) {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestWALHeaderFrameComesLast(t *testing.T) {
	// Regression: the header used to be written FIRST in the WAL, so a torn
	// append could persist a new header whose data frames were lost. The
	// header frame must double as the trailing commit record.
	path := t.TempDir() + "/db.ltsdb"
	ps := openNew(t, path)
	ps.Begin()
	ps.WriteBlob([]byte("x"))
	ps.MarkHeaderDirty()
	ordered := make([]int, 0, len(ps.dirty))
	for pageno := range ps.dirty {
		ordered = append(ordered, pageno)
	}
	if err := ps.appendWalFrames(ordered); err != nil {
		t.Fatal(err)
	}
	var order []int
	wal, err := os.Open(path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 16)
	for {
		_, err := ioReadFull(wal, head)
		if err != nil {
			break
		}
		order = append(order, int(int32(binary.LittleEndian.Uint32(head[8:]))))
		img := make([]byte, PageSize)
		ioReadFull(wal, img)
	}
	wal.Close()
	ps.Close(false)
	if len(order) == 0 || order[len(order)-1] != 0 {
		t.Fatalf("header page must be last in WAL, order=%v", order)
	}
	for i := 1; i < len(order)-1; i++ {
		if order[i-1] >= order[i] {
			t.Fatalf("data pages must ascend, order=%v", order)
		}
	}
}

func TestWALWithoutHeaderFrameKeepsOldState(t *testing.T) {
	// A WAL holding only data frames (header frame lost to a torn tail) must
	// never advance the header: the old state stays intact.
	path := t.TempDir() + "/db.ltsdb"
	ps := openNew(t, path)
	ps.Begin()
	loc, _ := ps.WriteBlob([]byte("KEEP"))
	ps.Commit()
	keepWALOnDisk(t, ps)

	// Replace the WAL with a lone data frame for an arbitrary page.
	img := make([]byte, PageSize)
	wal, err := os.OpenFile(path+".wal", os.O_TRUNC|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 16)
	binary.LittleEndian.PutUint64(head[0:], 0)
	binary.LittleEndian.PutUint32(head[8:], 3)
	binary.LittleEndian.PutUint32(head[12:], 0) // CRC of zeros below
	wal.Write(head)
	wal.Write(img)
	wal.Close()

	ps2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ps2.Close(false)
	got, err := ps2.ReadBlob(loc)
	if err != nil || !bytes.Equal(got, []byte("KEEP")) {
		t.Fatalf("got %q err %v", got, err)
	}
}
