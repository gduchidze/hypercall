#!/usr/bin/env python3
"""Install a verified wheel bundle into a template tree, without pip or networking."""
from __future__ import annotations
import argparse
import email.parser
import hashlib
import importlib.machinery
import io
import json
from pathlib import Path, PurePosixPath
import re
import stat
import tarfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]

def digest(data):
    return hashlib.sha256(data).hexdigest()

def safe_path(name):
    path = PurePosixPath(name)
    if (not name or len(name) > 1024 or path.is_absolute() or '..' in path.parts
            or '\\' in name or '\x00' in name or str(path) != name.rstrip('/')):
        raise ValueError('unsafe offline bundle path')
    return path

def supports_python(spec):
    if not spec:
        return False
    current = (3, 12, 0)
    for clause in spec.split(','):
        match = re.fullmatch(r'\s*(>=|<=|==|!=|>|<)\s*(\d+)\.(\d+)(?:\.(\d+))?\s*', clause)
        if not match:
            raise ValueError('unsupported Requires-Python constraint')
        operator, major, minor, patch = match.groups()
        version = (int(major), int(minor), int(patch or 0))
        if not {'>=': current >= version, '<=': current <= version,
                '>': current > version, '<': current < version,
                '==': current == version, '!=': current != version}[operator]:
            return False
    return True

def install_bundle(bundle, expected, architecture, tree, wheel_lock=None):
    tree = Path(tree)
    wheel_lock = Path(wheel_lock or ROOT / 'deploy/runtime/harness-wheels.lock.json')
    raw = Path(bundle).read_bytes()
    if len(raw) > 128 << 20 or digest(raw) != expected:
        raise ValueError('harness bundle SHA-256/size mismatch')
    files = {}
    total = 0
    with tarfile.open(fileobj=io.BytesIO(raw), mode='r:gz') as archive:
        for member in archive:
            safe_path(member.name)
            total += member.size
            if not member.isfile() or total > 128 << 20 or member.name in files:
                raise ValueError('bundle requires unique bounded regular files')
            files[member.name] = archive.extractfile(member).read()
    manifest = json.loads(files.get('manifest.json', b''))
    if (manifest.get('schema_version') != 1 or manifest.get('architecture') != architecture
            or manifest.get('python_version') != '3.12' or manifest.get('python_abi') != 'cp312'
            or manifest.get('wheel_lock_sha256') != digest(wheel_lock.read_bytes())):
        raise ValueError('harness bundle interpreter/architecture/lock mismatch')
    lock = json.loads(wheel_lock.read_bytes())
    pinned = {entry['distribution']: entry for entry in lock['architectures'][architecture]}
    entries = manifest['wheels']
    if len(entries) != 4 or {e['distribution'] for e in entries} != set(pinned) | {'hypercall-sdk'}:
        raise ValueError('offline bundle must contain exactly the locked SDK dependencies')
    launcher = manifest['launcher']
    if launcher['filename'] != 'agent.py':
        raise ValueError('unexpected cooperative launcher')
    launcher_data = files.get('agent.py', b'')
    if digest(launcher_data) != launcher['sha256'] or len(launcher_data) != launcher['size']:
        raise ValueError('cooperative launcher integrity mismatch')
    expected_files = {'manifest.json', 'agent.py'} | {'wheels/' + e['filename'] for e in entries}
    if set(files) != expected_files:
        raise ValueError('unexpected offline bundle members')
    extracted = {}
    total = 0
    for entry in entries:
        if entry['distribution'] in pinned and entry != pinned[entry['distribution']]:
            raise ValueError('wheel differs from pinned dependency lock')
        filename = entry['filename']
        if str(safe_path(filename)) != filename or '/' in filename or not filename.endswith('.whl'):
            raise ValueError('unsafe wheel filename')
        data = files['wheels/' + filename]
        if digest(data) != entry['sha256'] or len(data) != entry['size']:
            raise ValueError('offline wheel integrity mismatch')
        metadata = []
        notices = False
        with zipfile.ZipFile(io.BytesIO(data)) as wheel:
            seen = set()
            for member in wheel.infolist():
                name = str(safe_path(member.filename))
                mode = member.external_attr >> 16
                if member.is_dir():
                    continue
                if ((stat.S_IFMT(mode) not in (0, stat.S_IFREG)) or name in seen
                        or name in extracted or any(part.endswith('.data') for part in PurePosixPath(name).parts)):
                    raise ValueError('unsafe or colliding offline wheel member')
                seen.add(name)
                total += member.file_size
                if total > 256 << 20 or member.file_size > 64 << 20:
                    raise ValueError('expanded offline wheels exceed bounds')
                body = wheel.read(member)
                if '.dist-info/' in name and name.endswith('/METADATA'):
                    metadata.append(email.parser.BytesParser().parsebytes(body))
                if 'license' in name.lower() or 'copying' in name.lower():
                    notices = True
                extracted[name] = body
        if len(metadata) != 1 or not notices:
            raise ValueError('wheel metadata or license material missing')
        meta = metadata[0]
        normalize = lambda value: re.sub(r'[-_.]+', '-', value).lower()
        if normalize(meta['Name']) != normalize(entry['distribution']) or meta['Version'] != entry['version']:
            raise ValueError('wheel distribution/version metadata mismatch')
        if not supports_python(meta.get('Requires-Python')):
            raise ValueError('wheel Requires-Python excludes actual CPython 3.12 image')
    destination = tree / 'opt/hypercall'
    destination.mkdir(parents=True, exist_ok=False)
    destination.chmod(0o755)
    site = destination / 'python'
    site.mkdir(mode=0o755)
    for name, body in extracted.items():
        target = site.joinpath(*PurePosixPath(name).parts)
        target.parent.mkdir(parents=True, exist_ok=True)
        for parent in (target.parent, *target.parents):
            if parent == destination:
                break
            parent.chmod(0o755)
        target.write_bytes(body)
        target.chmod(0o644)
    for name, body in [('agent.py', launcher_data), ('bundle-manifest.json', files['manifest.json'])]:
        target = destination / name
        target.write_bytes(body)
        target.chmod(0o644)
    return manifest

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle', type=Path, required=True)
    parser.add_argument('--sha256', required=True)
    parser.add_argument('--arch', choices=['arm64', 'amd64'], required=True)
    parser.add_argument('--root', type=Path, required=True)
    args = parser.parse_args()
    install_bundle(args.bundle, args.sha256, args.arch, args.root)

if __name__ == '__main__':
    main()
