#!/usr/bin/env python3
"""Deterministic archive helper for the release scripts (Python 3.8+).

  release_tool.py archive --format tar.gz|zip --epoch N --out FILE --add SRC NAME MODE ...
  release_tool.py list ARCHIVE         one "NAME MODE KIND" line per member
  release_tool.py extract ARCHIVE DEST regular files only; unsafe names are refused
  release_tool.py arch FILE            elf-*, pe-* or macho-* token for an executable

Archives are written by this tool instead of tar(1)/zip(1) so the bytes do not
depend on the host: members are sorted by name, owned by 0/0, stamped with the
given epoch and have no directory entries (extracting one onto / would rewrite
the mode of /etc and /usr on a router). Paths are passed relative to the current
directory so Windows path conversion never touches them.
"""
import argparse
import gzip
import io
import os
import stat
import struct
import sys
import tarfile
import time
import zipfile


def die(message):
    sys.exit("release_tool: " + message)


def safe_name(name):
    parts = name.split("/")
    return bool(name) and not name.startswith("/") and ":" not in name \
        and "\\" not in name and ".." not in parts and "" not in parts


def read_entries(items):
    entries = []
    for src, name, mode in items:
        if not safe_name(name):
            die("unsafe member name: " + name)
        with open(src, "rb") as handle:
            entries.append((name, int(mode, 8), handle.read()))
    if not entries:
        die("no members given")
    return sorted(entries)


def write_targz(out, epoch, entries):
    with open(out, "wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0, compresslevel=9) as gz:
            with tarfile.open(fileobj=gz, mode="w", format=tarfile.GNU_FORMAT) as tar:
                for name, mode, data in entries:
                    info = tarfile.TarInfo(name)
                    info.size = len(data)
                    info.mtime = epoch
                    info.mode = mode
                    info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    tar.addfile(info, io.BytesIO(data))


def write_zip(out, epoch, entries):
    stamp = time.gmtime(epoch)[:6]
    if stamp[0] < 1980:
        die("zip cannot store a date before 1980")
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, mode, data in entries:
            info = zipfile.ZipInfo(name, date_time=stamp)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | mode) << 16
            archive.writestr(info, data)


def cmd_archive(args):
    entries = read_entries(args.add)
    if args.format == "tar.gz":
        write_targz(args.out, args.epoch, entries)
    else:
        write_zip(args.out, args.epoch, entries)


def sniff(path):
    with open(path, "rb") as handle:
        magic = handle.read(4)
    if magic[:2] == b"\x1f\x8b":
        return "tar.gz"
    if magic[:4] == b"PK\x03\x04":
        return "zip"
    die("not a tar.gz or zip archive: " + path)


def members(path):
    """Yield (name, mode, kind, read) for every member; kind is f, d or other."""
    if sniff(path) == "tar.gz":
        with tarfile.open(path, "r:gz") as tar:
            for member in tar:
                kind = "f" if member.isreg() else "d" if member.isdir() else "other"
                yield member.name, member.mode & 0o7777, kind, \
                    (lambda m=member: tar.extractfile(m).read())
    else:
        with zipfile.ZipFile(path) as archive:
            for info in archive.infolist():
                kind = "d" if info.filename.endswith("/") else "f"
                yield info.filename, (info.external_attr >> 16) & 0o7777, kind, \
                    (lambda i=info: archive.read(i))


def cmd_list(args):
    for name, mode, kind, _ in members(args.archive):
        print(name, format(mode, "03o"), kind)


def cmd_extract(args):
    for name, mode, kind, read in members(args.archive):
        if kind != "f":
            die("refusing non-file member: " + name)
        if not safe_name(name):
            die("unsafe member name: " + name)
        target = os.path.join(args.dest, *name.split("/"))
        os.makedirs(os.path.dirname(target), exist_ok=True)
        with open(target, "wb") as handle:
            handle.write(read())
        if mode & 0o111:
            os.chmod(target, 0o755)

ELF_MACHINES = {3: "386", 62: "x86_64", 40: "arm", 183: "aarch64"}
PE_MACHINES = {0x14C: "386", 0x8664: "amd64", 0xAA64: "arm64"}
MACHO_CPUS = {0x01000007: "x86_64", 0x0100000C: "arm64"}


def executable_format(path):
    with open(path, "rb") as handle:
        head = handle.read(4096)
    if head[:4] == b"\x7fELF" and len(head) >= 20:
        machine = struct.unpack_from("<H", head, 18)[0]
        return "elf-" + ELF_MACHINES.get(machine, "unknown")
    if head[:2] == b"MZ" and len(head) >= 64:
        offset = struct.unpack_from("<I", head, 0x3C)[0]
        if offset + 6 <= len(head) and head[offset:offset + 4] == b"PE\0\0":
            machine = struct.unpack_from("<H", head, offset + 4)[0]
            return "pe-" + PE_MACHINES.get(machine, "unknown")
    if head[:4] == b"\xcf\xfa\xed\xfe" and len(head) >= 8:
        cpu = struct.unpack_from("<I", head, 4)[0]
        return "macho-" + MACHO_CPUS.get(cpu, "unknown")
    return "unknown"


def cmd_arch(args):
    print(executable_format(args.file))


def main():
    # Keep "\n" on Windows so shell comparisons never see a stray "\r".
    sys.stdout.reconfigure(newline="\n")

    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    commands = parser.add_subparsers(dest="command", required=True)

    archive = commands.add_parser("archive")
    archive.add_argument("--format", choices=["tar.gz", "zip"], required=True)
    archive.add_argument("--epoch", type=int, required=True)
    archive.add_argument("--out", required=True)
    archive.add_argument("--add", nargs=3, action="append", default=[],
                         metavar=("SRC", "NAME", "MODE"))
    archive.set_defaults(run=cmd_archive)

    listing = commands.add_parser("list")
    listing.add_argument("archive")
    listing.set_defaults(run=cmd_list)

    extract = commands.add_parser("extract")
    extract.add_argument("archive")
    extract.add_argument("dest")
    extract.set_defaults(run=cmd_extract)

    arch = commands.add_parser("arch")
    arch.add_argument("file")
    arch.set_defaults(run=cmd_arch)

    args = parser.parse_args()
    args.run(args)


if __name__ == "__main__":
    main()
