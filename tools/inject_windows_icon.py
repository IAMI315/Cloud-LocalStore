"""Inject a PNG-backed ICO as a Windows RT_ICON/RT_GROUP_ICON resource."""

from __future__ import annotations

import struct
import sys
from pathlib import Path


ALIGN = lambda value, unit: (value + unit - 1) // unit * unit


class Resources:
    def __init__(self, ico: bytes):
        self.data = bytearray()
        self.data_entries: list[tuple[int, int, int]] = []
        self.frames = []
        count = struct.unpack_from("<H", ico, 4)[0]
        for index in range(count):
            off = 6 + index * 16
            width, height, colors, reserved, planes, bits, size, image_off = struct.unpack_from("<BBBBHHII", ico, off)
            image = ico[image_off:image_off + size]
            self.frames.append((width, height, colors, reserved, planes, bits, image))

    def blob(self, value: bytes) -> int:
        start = ALIGN(len(self.data), 4)
        self.data.extend(b"\0" * (start - len(self.data)))
        self.data.extend(value)
        return start

    def data_entry(self, blob_offset: int, size: int) -> int:
        start = self.blob(b"\0" * 16)
        self.data_entries.append((start, blob_offset, size))
        return start

    def directory(self, entries: list[tuple[int, bool, int]]) -> int:
        start = self.blob(b"\0" * (16 + 8 * len(entries)))
        struct.pack_into("<HHHHHHHH", self.data, start, 0, 0, 0, 0, 0, 0, 0, len(entries))
        for index, (identifier, is_directory, target) in enumerate(entries):
            entry = start + 16 + index * 8
            struct.pack_into("<II", self.data, entry, identifier, (0x80000000 if is_directory else 0) | target)
        return start

    def build(self, section_rva: int) -> tuple[bytes, int]:
        root = self.blob(b"\0" * 32)
        struct.pack_into("<HHHHHHHH", self.data, root, 0, 0, 0, 0, 0, 0, 0, 2)
        icon_id_entries = []
        group_entries = []
        for icon_id, frame in enumerate(self.frames, 1):
            image_offset = self.blob(frame[6])
            image_data = self.data_entry(image_offset, len(frame[6]))
            language = self.directory([(1033, False, image_data)])
            icon_id_entries.append((icon_id, True, language))
            group_entries.append((frame[0], frame[1], frame[2], frame[3], frame[4], frame[5], len(frame[6]), icon_id))

        icons = self.directory(icon_id_entries)
        icon_type = self.directory([(3, True, icons)])

        group = struct.pack("<HHH", 0, 1, len(group_entries))
        for width, height, colors, reserved, planes, bits, size, icon_id in group_entries:
            group += struct.pack("<BBBBHHIH", width, height, colors, reserved, planes, bits, size, icon_id)
        group_offset = self.blob(group)
        group_data = self.data_entry(group_offset, len(group))
        group_name = self.directory([(1033, False, group_data)])
        groups = self.directory([(1, True, group_name)])
        group_type = self.directory([(14, True, groups)])

        struct.pack_into("<II", self.data, root + 16, 3, 0x80000000 | icon_type)
        struct.pack_into("<II", self.data, root + 24, 14, 0x80000000 | group_type)
        for entry, blob_offset, size in self.data_entries:
            struct.pack_into("<IIII", self.data, entry, section_rva + blob_offset, size, 0, 0)
        return bytes(self.data), root


def patch_executable(exe: Path, ico_path: Path) -> None:
    binary = bytearray(exe.read_bytes())
    pe = struct.unpack_from("<I", binary, 0x3C)[0]
    if binary[pe:pe + 4] != b"PE\0\0":
        raise RuntimeError("not a PE executable")
    section_count = struct.unpack_from("<H", binary, pe + 6)[0]
    optional_size = struct.unpack_from("<H", binary, pe + 20)[0]
    optional = pe + 24
    if struct.unpack_from("<H", binary, optional)[0] != 0x20B:
        raise RuntimeError("only PE32+ executables are supported")
    section_alignment, file_alignment = struct.unpack_from("<II", binary, optional + 32)
    size_of_headers = struct.unpack_from("<I", binary, optional + 60)[0]
    section_table = optional + optional_size
    last = section_table + (section_count - 1) * 40
    last_va, last_virtual_size, last_raw_ptr, last_raw_size = struct.unpack_from("<IIII", binary, last + 8)
    new_va = ALIGN(last_va + last_virtual_size, section_alignment)
    raw_ptr = ALIGN(len(binary), file_alignment)
    resources = Resources(ico_path.read_bytes())
    resource_data, root_offset = resources.build(new_va)
    raw_size = ALIGN(len(resource_data), file_alignment)
    new_section = section_table + section_count * 40
    if new_section + 40 > size_of_headers:
        raise RuntimeError("PE headers have no room for a resource section")
    binary.extend(b"\0" * (raw_ptr - len(binary)))
    binary.extend(resource_data)
    binary.extend(b"\0" * (raw_size - len(resource_data)))
    name = b".rsrc\0\0\0"
    struct.pack_into("<8sIIIIIIHHI", binary, new_section, name, len(resource_data), new_va, raw_size, raw_ptr, 0, 0, 0, 0, 0x40000040)
    struct.pack_into("<H", binary, pe + 6, section_count + 1)
    # IMAGE_DIRECTORY_ENTRY_RESOURCE is data directory index 2.
    struct.pack_into("<II", binary, optional + 112 + 8 * 2, new_va, len(resource_data))
    struct.pack_into("<I", binary, optional + 56, ALIGN(new_va + len(resource_data), section_alignment))
    exe.write_bytes(binary)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: inject_windows_icon.py <exe> <ico>")
    patch_executable(Path(sys.argv[1]), Path(sys.argv[2]))
