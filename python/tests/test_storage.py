"""Unit tests for :mod:`tsdb.storage` (single-file page store + WAL)."""
import os
import shutil
import struct
import tempfile
import unittest

from tsdb import storage as S


class StorageTestBase(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="ltdb_test_")

    def tearDown(self):
        shutil.rmtree(self.dir, ignore_errors=True)

    def path(self, name="db.ltdb"):
        return os.path.join(self.dir, name)


class TestHeader(StorageTestBase):
    def test_fresh_file_header_fields(self):
        ps = S.PageStore(self.path())
        self.assertEqual(ps.header.total_pages, 1)
        self.assertEqual(ps.header.free_list_head, -1)
        self.assertEqual(ps.header.root_index_page, -1)
        ps.close()

    def test_header_checksum_corruption_detected(self):
        p = self.path()
        S.PageStore(p).close()
        # Flip a byte inside created_at (offset 8..15) to break the checksum.
        with open(p, "r+b") as f:
            f.seek(9)
            f.write(b"\xff")
        with self.assertRaises(S.StorageError):
            S.PageStore(p)


class TestBlob(StorageTestBase):
    def test_small_blob_roundtrip_reopen(self):
        p = self.path()
        ps = S.PageStore(p)
        ps.begin()
        loc = ps.write_blob(b"hello world")
        ps.commit()
        self.assertEqual(ps.read_blob(loc), b"hello world")
        ps.close()
        with S.PageStore(p) as ps2:
            self.assertEqual(ps2.read_blob(loc), b"hello world")

    def test_multi_page_blob_chaining(self):
        # Larger than one page's capacity (PAGE_CAP = 4088) forces chaining.
        p = self.path()
        payload = bytes(range(256)) * 30          # ~7680 bytes -> 2+ pages
        ps = S.PageStore(p)
        ps.begin()
        loc = ps.write_blob(payload)
        ps.commit()
        self.assertEqual(ps.read_blob(loc), payload)
        ps.close()
        with S.PageStore(p) as ps2:
            self.assertEqual(ps2.read_blob(loc), payload)

    def test_empty_blob_rejected(self):
        p = self.path()
        ps = S.PageStore(p)
        ps.begin()
        with self.assertRaises(ValueError):
            ps.write_blob(b"")
        ps.close()


class TestFreeList(StorageTestBase):
    def test_alloc_free_reuse(self):
        p = self.path()
        ps = S.PageStore(p)
        a = ps.alloc_page()
        b = ps.alloc_page()
        ps.free_page(a)
        ps.free_page(b)
        c = ps.alloc_page()          # should reuse the most recently freed (b)
        self.assertEqual(c, b)
        d = ps.alloc_page()         # then a
        self.assertEqual(d, a)
        ps.close()


class TestWalRecovery(StorageTestBase):
    def test_replay_restores_lost_data_page(self):
        p = self.path()
        data = b"CRASHDATA" * 10
        ps = S.PageStore(p)
        ps.begin()
        loc = ps.write_blob(data)
        ps.commit()                    # main + WAL now consistent
        ps.close(checkpoint=False)     # keep the WAL on disk

        # Simulate a crash that lost the data page from the main file.
        with open(p, "r+b") as f:
            f.seek(S.PAGE_SIZE)       # zero out page 1 (the blob page)
            f.write(b"\x00" * S.PAGE_SIZE)

        # Reopen replays the WAL and restores the lost page.
        with S.PageStore(p) as ps2:
            self.assertEqual(ps2.read_blob(loc), data)

    def test_torn_wal_frame_is_ignored(self):
        p = self.path()
        ps = S.PageStore(p)
        ps.begin()
        loc = ps.write_blob(b"STILL-HERE")
        ps.commit()
        ps.close(checkpoint=False)

        # Append a truncated, corrupt frame to the WAL.
        with open(p + ".wal", "ab") as w:
            w.write(struct.pack("<QII", 999, 1, 0xDEADBEEF))
            w.write(b"\x00" * 16)     # far shorter than a full page image

        # Reopen must stop at the bad frame and keep prior good data.
        with S.PageStore(p) as ps2:
            self.assertEqual(ps2.read_blob(loc), b"STILL-HERE")

    def test_wal_header_frame_comes_last(self):
        # Regression: the header used to be written FIRST in the WAL, so a
        # torn append could persist a new header whose data frames were lost.
        # The header frame must double as the trailing commit record.
        import zlib
        p = self.path()
        ps = S.PageStore(p)
        ps.begin()
        ps.write_blob(b"x")
        ps.write_page(0, ps.header.to_bytes())
        ps._append_wal_frames(sorted(ps._dirty.keys()))
        order = []
        with open(p + ".wal", "rb") as w:
            while True:
                head = w.read(16)
                if len(head) < 16:
                    break
                _seq, pageno, _crc = struct.unpack("<QII", head)
                order.append(pageno)
                w.read(S.PAGE_SIZE)
        ps.close()
        self.assertEqual(order[-1], 0)                      # header last
        self.assertEqual(sorted(order[:-1]), order[:-1])    # data ascending

    def test_wal_without_header_frame_keeps_old_state(self):
        # A WAL holding only data frames (header frame lost to a torn tail)
        # must never advance the header: the old state stays intact.
        import zlib
        p = self.path()
        ps = S.PageStore(p)
        ps.begin()
        loc = ps.write_blob(b"KEEP")
        ps.commit()
        ps.close(checkpoint=False)
        # Replace the WAL with a lone data frame for an arbitrary page.
        img = bytes(S.PAGE_SIZE)
        with open(p + ".wal", "wb") as w:
            w.write(struct.pack("<QII", 0, 3, zlib.crc32(img) & 0xFFFFFFFF))
            w.write(img)
        with S.PageStore(p) as ps2:
            self.assertEqual(ps2.read_blob(loc), b"KEEP")


if __name__ == "__main__":
    unittest.main()
