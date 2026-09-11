#!/usr/bin/env python3
"""Verify that a PE executable contains Windows icon resource types."""

from __future__ import annotations

import argparse
import struct
import sys
from pathlib import Path

RT_ICON = 3
RT_GROUP_ICON = 14
IMAGE_DIRECTORY_ENTRY_RESOURCE = 2


def read_u16(data: bytes, offset: int) -> int:
    return struct.unpack_from("<H", data, offset)[0]


def read_u32(data: bytes, offset: int) -> int:
    return struct.unpack_from("<I", data, offset)[0]


def rva_to_offset(rva: int, sections: list[tuple[int, int, int]]) -> int:
    for virtual_address, virtual_size, raw_offset in sections:
        if virtual_address <= rva < virtual_address + virtual_size:
            return raw_offset + rva - virtual_address
    raise ValueError(f"resource RVA 0x{rva:x} is outside PE sections")


def resource_types(path: Path) -> set[int]:
    data = path.read_bytes()
    if data[:2] != b"MZ":
        raise ValueError("not an MZ executable")
    pe_offset = read_u32(data, 0x3C)
    if data[pe_offset : pe_offset + 4] != b"PE\0\0":
        raise ValueError("missing PE signature")
    section_count = read_u16(data, pe_offset + 6)
    optional_offset = pe_offset + 24
    optional_magic = read_u16(data, optional_offset)
    if optional_magic == 0x20B:
        data_directory_offset = optional_offset + 112
    elif optional_magic == 0x10B:
        data_directory_offset = optional_offset + 96
    else:
        raise ValueError(f"unsupported optional header 0x{optional_magic:x}")
    resource_rva = read_u32(data, data_directory_offset + 8 * IMAGE_DIRECTORY_ENTRY_RESOURCE)
    if resource_rva == 0:
        return set()
    section_offset = optional_offset + read_u16(data, pe_offset + 20)
    sections = []
    for index in range(section_count):
        offset = section_offset + index * 40
        sections.append((read_u32(data, offset + 12), read_u32(data, offset + 8), read_u32(data, offset + 20)))
    resource_offset = rva_to_offset(resource_rva, sections)
    named_entries = read_u16(data, resource_offset + 12)
    id_entries = read_u16(data, resource_offset + 14)
    types: set[int] = set()
    for index in range(named_entries, named_entries + id_entries):
        entry_offset = resource_offset + 16 + index * 8
        identifier = read_u32(data, entry_offset)
        if identifier & 0x80000000 == 0:
            types.add(identifier)
    return types


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("executable", type=Path)
    args = parser.parse_args()
    try:
        types = resource_types(args.executable)
    except (OSError, ValueError, struct.error) as error:
        print(f"PE icon check failed: {error}", file=sys.stderr)
        return 2
    missing = {RT_ICON, RT_GROUP_ICON} - types
    if missing:
        print(f"PE icon check failed: missing resource type(s) {sorted(missing)}; found {sorted(types)}", file=sys.stderr)
        return 1
    print(f"PE icon check passed: RT_ICON and RT_GROUP_ICON found in {args.executable}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
