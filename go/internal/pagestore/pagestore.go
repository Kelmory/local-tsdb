// Package pagestore implements the single-file, page-oriented storage engine
// (SQLite-inspired): 4096-byte pages, a header page, a linked freelist,
// chained overflow pages for blobs, and a physical write-ahead log with
// torn-frame-safe replay.
//
// On-disk format "LTS1" (all integers little-endian):
//
//	Header page (page 0):
//	  0  magic "LTS1"           4 bytes
//	  4  format_version = 1     u16
//	  6  page_size = 4096       u16
//	  8  reserved               8 bytes (zero)
//	 16  created_at_unix_ns     i64
//	 24  last_write_ns          i64
//	 32  total_pages            u32 (high-water mark; file starts at 1)
//	 36  free_list_head         i32 (-1 = empty)
//	 40  root_index_page        i64 (-1 = none)
//	 48  wal_seq                u32
//	 52  crc32 of bytes [0,52)  u32
//	 56..4095  zero padding
//
//	Content page: u32 next_page (0xFFFFFFFF = last) ; u32 payload_len ; payload
//	WAL frame:    u64 seq ; u32 pageno ; u32 crc32(image) ; 4096B page image
package pagestore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"time"
)

// Page geometry and format constants.
const (
	PageSize = 4096
	Magic    = "LTS1"
	Version  = 1

	pageHdrSize = 8                      // next_page u32 + payload_len u32
	PageCap     = PageSize - pageHdrSize // usable bytes per content page

	FreeNone     = int64(-1)
	freeSentinel = uint32(0xFFFFFFFF) // on-disk "no next page" marker

	hdrBodyLen = 52
)

// ErrCorrupt signals a corrupt or inconsistent on-disk structure.
var ErrCorrupt = errors.New("storage corrupt")

// Header mirrors the bookkeeping kept in page 0.
type Header struct {
	CreatedAt     int64
	LastWriteAt   int64
	TotalPages    uint32 // high-water mark (page count)
	FreeListHead  int64  // page number or FreeNone
	RootIndexPage int64  // start page of the index blob, or FreeNone
	WalSeq        uint32
}

// FreshHeader returns the header of a newly created database.
func FreshHeader() Header {
	return Header{
		CreatedAt:     time.Now().UnixNano(),
		TotalPages:    1,
		FreeListHead:  FreeNone,
		RootIndexPage: FreeNone,
	}
}

func (h Header) toPage() []byte {
	page := make([]byte, PageSize)
	copy(page[0:4], Magic)
	binary.LittleEndian.PutUint16(page[4:], Version)
	binary.LittleEndian.PutUint16(page[6:], PageSize)
	// bytes 8..15 reserved (already zero)
	binary.LittleEndian.PutUint64(page[16:], uint64(h.CreatedAt))
	binary.LittleEndian.PutUint64(page[24:], uint64(h.LastWriteAt))
	binary.LittleEndian.PutUint32(page[32:], h.TotalPages)
	binary.LittleEndian.PutUint32(page[36:], uint32(h.FreeListHead))
	binary.LittleEndian.PutUint64(page[40:], uint64(h.RootIndexPage))
	binary.LittleEndian.PutUint32(page[48:], h.WalSeq)
	binary.LittleEndian.PutUint32(page[52:], crc32.ChecksumIEEE(page[:hdrBodyLen]))
	return page
}

func headerFromPage(page []byte) (Header, error) {
	if len(page) < hdrBodyLen+4 {
		return Header{}, fmt.Errorf("%w: header page too short", ErrCorrupt)
	}
	if string(page[0:4]) != Magic {
		return Header{}, fmt.Errorf("%w: bad magic %q", ErrCorrupt, page[0:4])
	}
	if v := binary.LittleEndian.Uint16(page[4:]); v != Version {
		return Header{}, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, v)
	}
	stored := binary.LittleEndian.Uint32(page[52:])
	if calc := crc32.ChecksumIEEE(page[:hdrBodyLen]); stored != calc {
		return Header{}, fmt.Errorf("%w: header checksum mismatch", ErrCorrupt)
	}
	return Header{
		CreatedAt:     int64(binary.LittleEndian.Uint64(page[16:])),
		LastWriteAt:   int64(binary.LittleEndian.Uint64(page[24:])),
		TotalPages:    binary.LittleEndian.Uint32(page[32:]),
		FreeListHead:  int64(int32(binary.LittleEndian.Uint32(page[36:]))),
		RootIndexPage: int64(binary.LittleEndian.Uint64(page[40:])),
		WalSeq:        binary.LittleEndian.Uint32(page[48:]),
	}, nil
}

// PageStore manages the single database file and its write-ahead log.
// It is not safe for concurrent use; callers must serialize access.
type PageStore struct {
	Path    string
	walPath string
	f       *os.File
	Header  Header
	dirty   map[int][]byte
}

// Open creates or opens the database file, replaying any un-checkpointed WAL
// frames first (recover-then-checkpoint, SQLite-style).
func Open(path string) (*PageStore, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, err
		}
		if _, err := fh.Write(FreshHeader().toPage()); err != nil {
			fh.Close()
			return nil, err
		}
		if err := fh.Sync(); err != nil {
			fh.Close()
			return nil, err
		}
		fh.Close()
	}
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	ps := &PageStore{Path: path, walPath: path + ".wal", f: fh, dirty: map[int][]byte{}}
	if err := ps.replayWal(); err != nil {
		fh.Close()
		return nil, err
	}
	page, err := ps.readPageRaw(0)
	if err != nil {
		fh.Close()
		return nil, err
	}
	hdr, err := headerFromPage(page)
	if err != nil {
		fh.Close()
		return nil, err
	}
	ps.Header = hdr
	return ps, nil
}

// -- low-level page I/O ------------------------------------------------------

// ReadPage returns the current image of a page (dirty override wins; pages
// past EOF read as zero-filled, mirroring freshly allocated pages).
func (ps *PageStore) ReadPage(pageno int) []byte {
	if img, ok := ps.dirty[pageno]; ok {
		return img
	}
	img, _ := ps.readPageRaw(pageno)
	return img
}

func (ps *PageStore) readPageRaw(pageno int) ([]byte, error) {
	need := int64(pageno+1) * PageSize
	size, err := ps.f.Seek(0, os.SEEK_END)
	if err != nil {
		return nil, err
	}
	if need > size {
		return make([]byte, PageSize), nil
	}
	if _, err := ps.f.Seek(int64(pageno)*PageSize, os.SEEK_SET); err != nil {
		return nil, err
	}
	img := make([]byte, PageSize)
	if _, err := ioReadFull(ps.f, img); err != nil {
		return nil, fmt.Errorf("%w: short read on page %d", ErrCorrupt, pageno)
	}
	return img, nil
}

// WritePage stages a full-page image; applied to disk at Commit.
func (ps *PageStore) WritePage(pageno int, data []byte) error {
	if len(data) != PageSize {
		return fmt.Errorf("page image must be exactly %d bytes", PageSize)
	}
	img := make([]byte, PageSize)
	copy(img, data)
	ps.dirty[pageno] = img
	return nil
}

func (ps *PageStore) flushPage(pageno int, data []byte) error {
	if _, err := ps.f.Seek(int64(pageno)*PageSize, os.SEEK_SET); err != nil {
		return err
	}
	_, err := ps.f.Write(data)
	return err
}

// -- allocation / freelist ---------------------------------------------------

// AllocPage pops the freelist head if any, else extends the file.
func (ps *PageStore) AllocPage() (int, error) {
	if ps.Header.FreeListHead != FreeNone {
		pageno := int(ps.Header.FreeListHead)
		img := ps.ReadPage(pageno)
		raw := binary.LittleEndian.Uint32(img[0:])
		if raw == freeSentinel {
			ps.Header.FreeListHead = FreeNone
		} else {
			ps.Header.FreeListHead = int64(int32(raw))
		}
		return pageno, nil
	}
	pageno := int(ps.Header.TotalPages)
	ps.Header.TotalPages++
	return pageno, nil
}

// FreePage pushes pageno onto the free list (SQLite freelist semantics).
func (ps *PageStore) FreePage(pageno int) error {
	oldHead := ps.Header.FreeListHead
	img := make([]byte, PageSize)
	copy(img, ps.ReadPage(pageno))
	link := uint32(freeSentinel)
	if oldHead != FreeNone {
		link = uint32(int32(oldHead))
	}
	binary.LittleEndian.PutUint32(img[0:], link)
	if err := ps.WritePage(pageno, img); err != nil {
		return err
	}
	ps.Header.FreeListHead = int64(pageno)
	return nil
}

// -- blob API (chained pages) ------------------------------------------------

// WriteBlob persists data across as many whole pages as needed and returns
// the start page number.
func (ps *PageStore) WriteBlob(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("cannot store empty blob")
	}
	var pages []int
	rest := data
	for len(rest) > 0 || len(pages) == 0 {
		pageno, err := ps.AllocPage()
		if err != nil {
			return 0, err
		}
		pages = append(pages, pageno)
		chunk := rest
		if len(chunk) > PageCap {
			chunk = chunk[:PageCap]
		}
		rest = rest[len(chunk):]
		img := make([]byte, PageSize)
		binary.LittleEndian.PutUint32(img[0:], freeSentinel) // next=-1 placeholder
		binary.LittleEndian.PutUint32(img[4:], uint32(len(chunk)))
		copy(img[pageHdrSize:], chunk)
		if err := ps.WritePage(pageno, img); err != nil {
			return 0, err
		}
	}
	// Link the chain.
	for i := 0; i < len(pages)-1; i++ {
		img := make([]byte, PageSize)
		copy(img, ps.ReadPage(pages[i]))
		binary.LittleEndian.PutUint32(img[0:], uint32(pages[i+1]))
		if err := ps.WritePage(pages[i], img); err != nil {
			return 0, err
		}
	}
	return pages[0], nil
}

// ReadBlob walks the page chain and returns the payload bytes.
// Dirty (staged) pages are preferred, mirroring Python's read_page.
func (ps *PageStore) ReadBlob(start int) ([]byte, error) {
	var out []byte
	pageno := start
	for {
		img := ps.ReadPage(pageno)
		nxt := binary.LittleEndian.Uint32(img[0:])
		plen := int(binary.LittleEndian.Uint32(img[4:]))
		if plen < 0 || pageHdrSize+plen > PageSize {
			return nil, fmt.Errorf("%w: bad payload length on page %d", ErrCorrupt, pageno)
		}
		out = append(out, img[pageHdrSize:pageHdrSize+plen]...)
		if nxt == freeSentinel {
			break
		}
		pageno = int(int32(nxt))
	}
	return out, nil
}

// FreeBlob frees every page of a blob chain (in reverse order).
func (ps *PageStore) FreeBlob(start int) error {
	var pages []int
	pageno := start
	for {
		img := ps.ReadPage(pageno)
		nxt := binary.LittleEndian.Uint32(img[0:])
		pages = append(pages, pageno)
		if nxt == freeSentinel {
			break
		}
		pageno = int(int32(nxt))
	}
	for i := len(pages) - 1; i >= 0; i-- {
		if err := ps.FreePage(pages[i]); err != nil {
			return err
		}
	}
	return nil
}

// -- transactions / WAL ------------------------------------------------------

// Begin clears the staged (dirty) page set.
func (ps *PageStore) Begin() { ps.dirty = map[int][]byte{} }

// MarkHeaderDirty forces the header page to be rewritten on next Commit.
func (ps *PageStore) MarkHeaderDirty() error {
	return ps.WritePage(0, ps.Header.toPage())
}

// Commit durably persists the staged pages: WAL first (fsync), then main file.
func (ps *PageStore) Commit() error {
	if len(ps.dirty) == 0 {
		return nil
	}
	// Always (re)serialise the *current* header so bookkeeping made during
	// this transaction (alloc/free) is captured in page 0.
	ps.Header.LastWriteAt = time.Now().UnixNano()
	if err := ps.WritePage(0, ps.Header.toPage()); err != nil {
		return err
	}
	pages := make([]int, 0, len(ps.dirty))
	for pageno := range ps.dirty {
		pages = append(pages, pageno)
	}
	sortInts(pages)
	if err := ps.appendWalFrames(pages); err != nil {
		return err
	}
	if err := ps.applyToMain(pages); err != nil {
		return err
	}
	ps.Header.WalSeq += uint32(len(pages))
	ps.dirty = map[int][]byte{}
	return nil
}

// appendWalFrames appends full page images for pages to the WAL (fsync'd).
// The header page (0) goes LAST: replay applies frames in file order and
// stops at the first invalid frame, so a torn tail can never leave a new
// header pointing at data frames that were lost -- the header frame doubles
// as the commit record (SQLite-style).
func (ps *PageStore) appendWalFrames(pages []int) error {
	seq := ps.Header.WalSeq
	ordered := make([]int, 0, len(pages))
	hasHeader := false
	for _, p := range pages {
		if p == 0 {
			hasHeader = true
			continue
		}
		ordered = append(ordered, p)
	}
	if hasHeader {
		ordered = append(ordered, 0)
	}
	w, err := os.OpenFile(ps.walPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer w.Close()
	head := make([]byte, 16)
	for _, pageno := range ordered {
		image := ps.dirty[pageno]
		crc := crc32.ChecksumIEEE(image)
		binary.LittleEndian.PutUint64(head[0:], uint64(seq))
		binary.LittleEndian.PutUint32(head[8:], uint32(pageno))
		binary.LittleEndian.PutUint32(head[12:], crc)
		if _, err := w.Write(head); err != nil {
			return err
		}
		if _, err := w.Write(image); err != nil {
			return err
		}
		seq++
	}
	if err := w.Sync(); err != nil {
		return err
	}
	return w.Close()
}

// applyToMain writes staged pages to the main file (data first, header last).
func (ps *PageStore) applyToMain(pages []int) error {
	ordered := make([]int, 0, len(pages))
	headerPage := -1
	for _, p := range pages {
		if p == 0 {
			headerPage = 0
			continue
		}
		ordered = append(ordered, p)
	}
	if headerPage == 0 {
		ordered = append(ordered, 0)
	}
	for _, pageno := range ordered {
		if err := ps.flushPage(pageno, ps.dirty[pageno]); err != nil {
			return err
		}
	}
	if err := ps.f.Sync(); err != nil {
		return err
	}
	return nil
}

// Checkpoint removes the WAL once the main file is known to be up to date.
func (ps *PageStore) Checkpoint() error {
	if _, err := os.Stat(ps.walPath); err == nil {
		if err := removeRetry(ps.walPath); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the store; when checkpoint is true the WAL is removed first.
func (ps *PageStore) Close(checkpoint bool) error {
	var err error
	if checkpoint {
		err = ps.Checkpoint()
	}
	if cerr := ps.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// replayWal applies un-checkpointed WAL frames to the main file, then removes
// the WAL. A torn frame header, torn page image, or CRC mismatch stops replay.
func (ps *PageStore) replayWal() error {
	w, err := os.Open(ps.walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	applied := false
	head := make([]byte, 16)
	image := make([]byte, PageSize)
	for {
		if _, err := ioReadFull(w, head); err != nil {
			break // torn trailing header
		}
		pageno := int(int32(binary.LittleEndian.Uint32(head[8:])))
		crc := binary.LittleEndian.Uint32(head[12:])
		if _, err := ioReadFull(w, image); err != nil {
			break // torn page image -> stop
		}
		if crc32.ChecksumIEEE(image) != crc {
			break // corrupt frame -> stop
		}
		if err := ps.flushPage(pageno, image); err != nil {
			w.Close()
			return err
		}
		applied = true
	}
	if applied {
		if err := ps.f.Sync(); err != nil {
			return err
		}
	}
	info, err := w.Stat()
	closeErr := w.Close()
	if closeErr != nil {
		return closeErr
	}
	if err == nil && (applied || info.Size() == 0) {
		return removeRetry(ps.walPath)
	}
	return nil
}

// removeRetry removes a file, retrying once (Windows AV/file-lock races).
func removeRetry(path string) error {
	if err := os.Remove(path); err == nil {
		return nil
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func ioReadFull(f *os.File, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
