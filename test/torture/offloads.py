#!/usr/bin/env python3
"""Keep TCP at the simulated MTU inside the torture network namespace.

Otherwise HTB/netem schedule multi-MTU GSO packets as indivisible bursts.
Run under the same unshare -rn wrapper as torture.mjs; only its loopback changes.
"""
import ctypes
import fcntl
import json
import socket
import struct

SIOCETHTOOL = 0x8946
sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)


def feature(command, value=0):
    data = ctypes.create_string_buffer(struct.pack("II", command, value))
    request = struct.pack("16sP16x", b"lo", ctypes.addressof(data))
    fcntl.ioctl(sock, SIOCETHTOOL, request)
    return struct.unpack("II", data.raw[:8])[1]


results = []
for get, setter, name in [(0x1E, 0x1F, "tso"), (0x23, 0x24, "gso"), (0x2B, 0x2C, "gro")]:
    before = feature(get)
    feature(setter, 0)
    after = feature(get)
    results.append(dict(feature=name, before=before, after=after))
if any(result["after"] != 0 for result in results):
    raise SystemExit(json.dumps(results))
print(json.dumps(results))
