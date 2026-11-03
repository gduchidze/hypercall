import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class ArtifactCLI(unittest.TestCase):
    def test_rejects_tampered_cache_before_use(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            cache = directory / 'arm64'
            cache.mkdir()
            (cache / 'kernel').write_bytes(b'tampered')
            lock = directory / 'lock.json'
            lock.write_text(json.dumps({'schema_version': 1, 'architectures': {'arm64': {
                'kernel': {'url': 'https://invalid.example/kernel', 'sha256': hashlib.sha256(b'original').hexdigest()}
            }}}))
            result = subprocess.run(['python3', str(ROOT / 'scripts/fetch-runtime'), '--lock', str(lock),
                                     '--arch', 'arm64', '--cache', str(directory), '--offline'],
                                    capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('SHA-256 mismatch', result.stderr)
            self.assertEqual((cache / 'kernel').read_bytes(), b'tampered')

    def test_accepts_verified_offline_input_without_rewriting_it(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            cache = directory / 'arm64'
            cache.mkdir()
            path = cache / 'kernel'
            path.write_bytes(b'known locked kernel input')
            before = path.stat().st_mtime_ns
            lock = directory / 'lock.json'
            lock.write_text(json.dumps({'schema_version': 1, 'architectures': {'arm64': {
                'kernel': {'url': 'https://invalid.example/kernel', 'sha256': '30178f9f425e887359d789a93dece92abcc884e83f5cce7dd24f29c433a380af'}
            }}}))
            result = subprocess.run(['python3', str(ROOT / 'scripts/fetch-runtime'), '--lock', str(lock),
                                     '--arch', 'arm64', '--cache', str(directory), '--offline'],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(path.stat().st_mtime_ns, before)


if __name__ == '__main__':
    unittest.main()
