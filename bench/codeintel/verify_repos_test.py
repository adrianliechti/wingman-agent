import hashlib
from pathlib import Path
import tempfile
import unittest

from verify_repos import verify_tree


class VerifyRepositoriesTests(unittest.TestCase):
    def test_changed_missing_and_extra_sources_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root/"source.go"
            source.write_bytes(b"original")
            expected = {"source.go":hashlib.sha256(b"original").hexdigest()}
            self.assertEqual(len(verify_tree(root,expected)),64)
            source.write_bytes(b"changed")
            with self.assertRaisesRegex(ValueError,"changed"):
                verify_tree(root,expected)
            source.unlink()
            with self.assertRaisesRegex(ValueError,"missing"):
                verify_tree(root,expected)
            source.write_bytes(b"original")
            (root/"extra.go").write_bytes(b"extra")
            with self.assertRaisesRegex(ValueError,"unexpected"):
                verify_tree(root,expected)


if __name__ == "__main__":
    unittest.main()
