import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
import zipfile

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('harness_install', ROOT / 'scripts/install-harness-bundle.py')
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)

def sha(data):
    return hashlib.sha256(data).hexdigest()

def fixture(directory, requires='>=3.12,<3.14'):
    files, entries = {}, []
    for distribution in ('grpcio', 'protobuf', 'typing-extensions', 'hypercall-sdk'):
        name = distribution.replace('-', '_')
        body = io.BytesIO()
        with zipfile.ZipFile(body, 'w') as wheel:
            wheel.writestr(name + '/__init__.py', '# fixture only\n')
            wheel.writestr(name + '-1.0.dist-info/METADATA', 'Name: ' + distribution + '\nVersion: 1.0\nRequires-Python: ' + (requires if distribution == 'hypercall-sdk' else '>=3.9') + '\n')
            wheel.writestr(name + '-1.0.dist-info/licenses/LICENSE', 'fixture license')
        data = body.getvalue()
        entry = dict(distribution=distribution, version='1.0', filename=name + '-1.0-py3-none-any.whl', sha256=sha(data), size=len(data), url='fixture-only')
        files['wheels/' + entry['filename']] = data
        entries.append(entry)
    lock = directory / 'lock.json'
    lock.write_text(json.dumps({'architectures': {'arm64': entries[:3]}}))
    files['agent.py'] = b'# fixture launcher\n'
    manifest = dict(schema_version=1, python_abi='cp312', python_version='3.12', architecture='arm64', wheels=entries, wheel_lock_sha256=sha(lock.read_bytes()), launcher=dict(filename='agent.py', sha256=sha(files['agent.py']), size=len(files['agent.py'])))
    files['manifest.json'] = json.dumps(manifest).encode()
    bundle = directory / 'bundle.tar.gz'
    with tarfile.open(bundle, 'w:gz') as archive:
        for name, data in files.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            archive.addfile(info, io.BytesIO(data))
    return bundle, sha(bundle.read_bytes()), lock

class OfflineHarnessBundle(unittest.TestCase):
    def test_installs_pinned_wheels_with_distribution_license_material(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            bundle, digest, lock = fixture(directory)
            tree = directory / 'root'
            tree.mkdir()
            installer.install_bundle(bundle, digest, 'arm64', tree, lock)
            self.assertEqual((tree / 'opt/hypercall/python/hypercall_sdk-1.0.dist-info/licenses/LICENSE').read_text(), 'fixture license')
            self.assertEqual((tree / 'opt/hypercall/agent.py').read_bytes(), b'# fixture launcher\n')
    def test_rejects_sdk_requiring_python313_before_writing_image(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            bundle, digest, lock = fixture(directory, '>=3.13,<3.14')
            tree = directory / 'root'
            tree.mkdir()
            with self.assertRaisesRegex(ValueError, 'Requires-Python'):
                installer.install_bundle(bundle, digest, 'arm64', tree, lock)
            self.assertFalse((tree / 'opt/hypercall').exists())
    def test_rejects_wrong_bundle_digest_before_extracting(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            bundle, _, lock = fixture(directory)
            with self.assertRaisesRegex(ValueError, 'SHA-256'):
                installer.install_bundle(bundle, '0' * 64, 'arm64', directory / 'root', lock)

if __name__ == '__main__':
    unittest.main()
