"""Regression checks for the inode-preserving, narrowly scoped config patch."""
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("hotload", Path(__file__).with_name("hotload-company.py"))
hotload = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hotload)


class PatchTest(unittest.TestCase):
    def test_preserves_inode_and_unrelated_settings(self):
        original = "# unchanged comment\nserver:\n  port: 8318\nplugins:\n  configs:\n    cpa-codex-candy-eval:\n      store:\n        version: '0.4.4'\n        release-tag: v0.4.4\n"
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.yaml"
            path.write_text(original)
            inode = path.stat().st_ino
            old = {"version": "0.4.4", "release-tag": "v0.4.4"}
            new = {"version": "0.4.4-company.1", "release-tag": "v0.4.4-company.1"}
            hotload.patch_store(path, new, old)
            self.assertEqual(inode, path.stat().st_ino)
            self.assertTrue(path.read_text().startswith("# unchanged comment\nserver:\n  port: 8318\n"))
            path.write_text(path.read_text().replace("port: 8318", "port: 9000"))
            hotload.patch_store(path, old, new)
            self.assertIn("port: 9000", path.read_text())
            snapshot = path.read_text()
            with self.assertRaises(RuntimeError):
                hotload.patch_store(path, old, new)
            self.assertEqual(snapshot, path.read_text())


if __name__ == "__main__":
    unittest.main()
