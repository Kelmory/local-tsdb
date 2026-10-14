"""Single-file, page-oriented storage engine (SQLite-inspired).

Design goals taken from SQLite's single-file model:

* The whole database lives in **one file** made of fixed-size pages
  (:data:`PAGE_SIZE`, default 4096 bytes) plus an optional write-ahead log.
* Page 0 is a **header page** carrying magic/version/page-size and the
  bookkeeping SQLite keeps in its file header: total page count, the head of
  a **free list** of reusable pages, and the location of the index root.
* Freed pages are recycled through a linked free list instead of being
  dropped, exactly like SQLite's freelist.
* Durability comes from a physical **write-ahead log** (``<db>.wal``): every
  committed transaction appends full page images to the WAL before they are
  applied to the main file; on open we replay any un-checkpointed frames and
  then truncate the WAL -- the same recover-then-checkpoint flow SQLite uses.

On top of raw pages we expose a small *blob* API that chains whole pages with
an explicit ``next_page`` pointer (SQLite's overflow-page technique) so larger
records can span multiple pages transparently.
"""
from __future__ import annotations

import os
import struct
import zlib
from typing import Dict, List, Optional

PAGE_SIZE = 4096
MAGIC = b"LTDB"
VERSION = 2  # v2: null bitmaps in data blocks, u32 per-series block count

# Content-page header (first 8 bytes of every non-header page):
#   next_page : u32  (-1 == last page in a blob / not chained)
#   payload_len: u32 (bytes of live payload stored on this page)
_PAGE_HDR = struct.Struct(">II")
PAGE_CAP = PAGE_SIZE - _PAGE_HDR.size          # usable bytes per content page

FREE_NONE = -1


class StorageError(Exception):
    """Raised when the on-disk structure is corrupt or inconsistent."""


# --------------------------------------------------------------------------- #
# Header (de)serialisation -- lives in page 0
# --------------------------------------------------------------------------- #
# magic(4s) version(H) page_size(H) pad(8x) created_at(q) last_write_at(q)
# total_pages(I) free_list_head(i) root_index_page(q) wal_seq(I)
_HEADER_FMT = "<4sHH8xqqIiqI"
_HEADER_STRUCT = struct.Struct(_HEADER_FMT)
_HDR_LEN = _HEADER_STRUCT.size


class Header:
    __slots__ = ("created_at", "last_write_at", "total_pages",
                "free_list_head", "root_index_page", "wal_seq")

    def __init__(self, created_at=0, last_write_at=0, total_pages=1,
                 free_list_head=FREE_NONE, root_index_page=FREE_NONE, wal_seq=0):
        self.created_at = created_at
        self.last_write_at = last_write_at
        self.total_pages = total_pages          # high-water mark (page count)
        self.free_list_head = free_list_head   # page number or FREE_NONE
        self.root_index_page = root_index_page  # start page of the index blob
        self.wal_seq = wal_seq

    def to_bytes(self) -> bytes:
        body = _HEADER_STRUCT.pack(
            MAGIC, VERSION, PAGE_SIZE,
            self.created_at, self.last_write_at,
            self.total_pages, self.free_list_head,
            self.root_index_page, self.wal_seq,
        )
        checksum = zlib.crc32(body) & 0xFFFFFFFF
        page = body + struct.pack("<I", checksum)
        return page.ljust(PAGE_SIZE, b"\x00")

    @classmethod
    def from_bytes(cls, page: bytes) -> "Header":
        if len(page) < _HDR_LEN:
            raise StorageError("header page too short")
        (magic, version, page_size, created_at, last_write_at,
         total_pages, free_list_head, root_index_page, wal_seq) = \
            _HEADER_STRUCT.unpack_from(page, 0)
        if magic != MAGIC:
            raise StorageError("bad magic {!r}".format(magic))
        if version != VERSION:
            raise StorageError("unsupported version {}".format(version))
        (stored,) = struct.unpack_from("<I", page, _HDR_LEN)
        calc = zlib.crc32(page[:_HDR_LEN]) & 0xFFFFFFFF
        if stored != calc:
            raise StorageError("header checksum mismatch")
        h = cls(created_at, last_write_at, total_pages, free_list_head,
                root_index_page, wal_seq)
        return h


# --------------------------------------------------------------------------- #
# Page store + WAL
# --------------------------------------------------------------------------- #
class PageStore:
    """Manages the single database file and its write-ahead log."""

    def __init__(self, path: str):
        self.path = path
        self.wal_path = path + ".wal"
        if not os.path.exists(path):
            # Create a fresh file containing only the header page.
            with open(path, "wb") as f:
                f.write(Header(total_pages=1).to_bytes())
        try:
            self._fh = open(path, "r+b")
            self._dirty: Dict[int, bytes] = {}
            # Replay any un-checkpointed WAL frames (idempotent), then truncate.
            self._replay_wal()
            self.header = Header.from_bytes(self.read_page(0))
        except Exception:
            # Never leak the file handle if validation/replay fails.
            try:
                self._fh.close()
            except AttributeError:
                pass
            raise

    # -- low-level page I/O -------------------------------------------------- #
    def read_page(self, pageno: int) -> bytes:
        if pageno in self._dirty:
            return self._dirty[pageno]
        # A freshly allocated but not-yet-written page reads as zero-filled.
        need = (pageno + 1) * PAGE_SIZE
        size = os.fstat(self._fh.fileno()).st_size
        if need > size:
            return b"\x00" * PAGE_SIZE
        self._fh.seek(pageno * PAGE_SIZE)
        data = self._fh.read(PAGE_SIZE)
        if len(data) < PAGE_SIZE:
            raise StorageError("short read on page {}".format(pageno))
        return data

    def write_page(self, pageno: int, data: bytes) -> None:
        """Stage a full-page image; applied to disk at :meth:`commit`."""
        if len(data) != PAGE_SIZE:
            raise ValueError("page image must be exactly {} bytes".format(PAGE_SIZE))
        self._dirty[pageno] = data

    def _flush_page(self, pageno: int, data: bytes) -> None:
        """Write a page straight to the main file (bypassing the dirty set)."""
        self._fh.seek(pageno * PAGE_SIZE)
        self._fh.write(data)

    # -- allocation / free list --------------------------------------------- #
    def alloc_page(self) -> int:
        h = self.header
        if h.free_list_head != FREE_NONE:
            pageno = h.free_list_head
            raw = struct.unpack_from(">I", self.read_page(pageno), 0)[0]
            # 0xFFFFFFFF is the on-disk encoding of "no next free page".
            h.free_list_head = FREE_NONE if raw == 0xFFFFFFFF else raw
        else:
            pageno = h.total_pages
            h.total_pages += 1
        return pageno

    def free_page(self, pageno: int) -> None:
        """Push ``pageno`` onto the free list (SQLite freelist semantics)."""
        old_head = self.header.free_list_head
        img = bytearray(self.read_page(pageno))
        struct.pack_into(">I", img, 0, old_head if old_head != FREE_NONE else 0xFFFFFFFF)
        self.write_page(pageno, bytes(img))
        self.header.free_list_head = pageno

    # -- blob API (chained pages) ------------------------------------------ #
    def write_blob(self, data: bytes) -> int:
        """Persist ``data`` across as many whole pages as needed.

        Returns the start page number; read it back with :meth:`read_blob`.
        """
        if not data:
            raise ValueError("cannot store empty blob")
        cap = PAGE_CAP
        pages: List[int] = []
        while data or len(pages) == 0:
            pageno = self.alloc_page()
            pages.append(pageno)
            chunk, data = data[:cap], data[cap:]
            img = bytearray(PAGE_SIZE)
            _PAGE_HDR.pack_into(img, 0, 0xFFFFFFFF, len(chunk))  # next=-1 placeholder
            img[_PAGE_HDR.size:_PAGE_HDR.size + len(chunk)] = chunk
            self.write_page(pageno, bytes(img))
            if not data:
                break
        # Link the chain.
        for i in range(len(pages) - 1):
            img = bytearray(self.read_page(pages[i]))
            _PAGE_HDR.pack_into(img, 0, pages[i + 1], struct.unpack_from(">I", img, 4)[0])
            self.write_page(pages[i], bytes(img))
        return pages[0]

    def read_blob(self, start_pageno: int) -> bytes:
        out = bytearray()
        pageno = start_pageno
        while True:
            img = self.read_page(pageno)
            nxt, plen = _PAGE_HDR.unpack_from(img, 0)
            out += img[_PAGE_HDR.size:_PAGE_HDR.size + plen]
            if nxt == 0xFFFFFFFF:      # -1
                break
            pageno = nxt
        return bytes(out)

    def free_blob(self, start_pageno: int) -> None:
        """Free every page of a blob chain (in reverse order)."""
        pages: List[int] = []
        pageno = start_pageno
        while True:
            img = self.read_page(pageno)
            nxt, _plen = _PAGE_HDR.unpack_from(img, 0)
            pages.append(pageno)
            if nxt == 0xFFFFFFFF:
                break
            pageno = nxt
        for p in reversed(pages):
            self.free_page(p)

    # -- transactions / WAL ------------------------------------------------- #
    def begin(self) -> None:
        self._dirty.clear()

    def mark_header_dirty(self) -> None:
        """Force the header page to be (re)written on next commit."""
        self.write_page(0, self.header.to_bytes())

    def commit(self) -> None:
        """Durably persist the staged pages (WAL first, then main file)."""
        if not self._dirty:
            return
        import time as _time
        # Always (re)serialise the *current* header so bookkeeping made during
        # this transaction (alloc/free) is captured in page 0.
        self.header.last_write_at = _time.time_ns()
        self.write_page(0, self.header.to_bytes())
        pages = sorted(self._dirty.keys())
        self._append_wal_frames(pages)
        self._apply_to_main(pages)
        # Refresh header bookkeeping from the staged image.
        if 0 in self._dirty:
            self.header = Header.from_bytes(self._dirty[0])
        self.header.wal_seq += len(pages)
        self._dirty.clear()

    def _append_wal_frames(self, pages) -> None:
        """Append full page images for ``pages`` to the WAL (fsync'd).

        The header page (0) goes LAST: replay applies frames in file order
        and stops at the first invalid frame, so a torn tail can never leave
        a new header pointing at data frames that were lost -- the header
        frame doubles as the commit record (SQLite-style).
        """
        seq = self.header.wal_seq
        ordered = sorted(p for p in pages if p != 0)
        if 0 in pages:
            ordered.append(0)
        with open(self.wal_path, "ab") as w:
            for pageno in ordered:
                image = self._dirty[pageno]
                crc = zlib.crc32(image) & 0xFFFFFFFF
                # ``seq`` is informational; replay applies every frame in order.
                w.write(struct.pack("<QII", seq, pageno, crc))
                w.write(image)
                seq += 1
            w.flush()
            os.fsync(w.fileno())

    def _apply_to_main(self, pages) -> None:
        """Write staged pages to the main file (data first, header last)."""
        for pageno in sorted(pages, key=lambda p: (p == 0)):
            self._flush_page(pageno, self._dirty[pageno])
        self._fh.flush()
        os.fsync(self._fh.fileno())

    def checkpoint(self) -> None:
        """Truncate the WAL once the main file is known to be up to date."""
        if os.path.exists(self.wal_path):
            # Only safe after a commit has applied every frame to the main file.
            os.remove(self.wal_path)

    def __enter__(self) -> "PageStore":
        return self

    def __exit__(self, exc_type, exc, tb) -> bool:
        self.close()
        return False

    def close(self, checkpoint: bool = True) -> None:
        try:
            if checkpoint:
                self.checkpoint()
        finally:
            if self._fh:
                self._fh.close()
                self._fh = None

    # -- WAL replay --------------------------------------------------------- #
    def _replay_wal(self) -> None:
        """Apply un-checkpointed WAL frames to the main file, then truncate."""
        if not os.path.exists(self.wal_path):
            return
        frame_hdr = struct.Struct("<QII")
        applied_any = False
        with open(self.wal_path, "rb") as w:
            while True:
                head = w.read(frame_hdr.size)
                if len(head) < frame_hdr.size:
                    break                       # torn trailing header
                seq, pageno, crc = frame_hdr.unpack(head)
                image = w.read(PAGE_SIZE)
                if len(image) < PAGE_SIZE:
                    break                       # torn page image -> stop
                if (zlib.crc32(image) & 0xFFFFFFFF) != crc:
                    break                       # corrupt frame -> stop
                self._apply_replay_page(pageno, image)
                applied_any = True
        if applied_any or os.path.getsize(self.wal_path) == 0:
            # Main file now reflects every valid frame; drop the WAL.
            try:
                os.remove(self.wal_path)
            except FileNotFoundError:
                pass

    def _apply_replay_page(self, pageno: int, image: bytes) -> None:
        """Write one replayed page image; extends the file if needed."""
        self._fh.seek(pageno * PAGE_SIZE)
        self._fh.write(image)
