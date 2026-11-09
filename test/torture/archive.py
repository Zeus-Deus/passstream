#!/usr/bin/env python3
"""Archive exact JSON bytes losslessly; retain plain copies in ignored out/."""
import gzip
from pathlib import Path

root = Path(__file__).resolve().parent
raw = root / 'out' / 'raw-results'
raw.mkdir(parents=True, exist_ok=True)
for path in sorted((root / 'results').glob('*.json')):
    data = path.read_bytes()
    target = path.with_suffix('.json.gz')
    compressed = gzip.compress(data, mtime=0)
    assert gzip.decompress(compressed) == data
    target.write_bytes(compressed)
    destination = raw / path.name
    if destination.exists() and destination.read_bytes() != data:
        raise RuntimeError(f'refusing to replace distinct raw evidence: {destination}')
    path.rename(destination)
    print(f'{path.name}: {len(data)} -> {len(compressed)} bytes, verified lossless')
