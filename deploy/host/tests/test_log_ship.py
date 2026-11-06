import importlib.machinery
import importlib.util
import os
from pathlib import Path

import pytest


@pytest.mark.parametrize("rotation", ["inspection", "upload"])
def test_rotation_keeps_each_inode_and_offset_together(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, rotation: str
) -> None:
    loader = importlib.machinery.SourceFileLoader("log_ship", str(Path(__file__).parents[1] / "cb-log-ship"))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    assert spec is not None
    shipper = importlib.util.module_from_spec(spec)
    loader.exec_module(shipper)
    monkeypatch.setattr(shipper, "DATA", str(tmp_path))
    monkeypatch.setattr(shipper, "MAX_CHUNK", 4)
    active = tmp_path / "audit.jsonl"
    old, new = b"old\none\n", b"new\ntwo\n"
    active.write_bytes(old)
    old_ino = active.stat().st_ino
    rotated = False
    uploaded: dict[int, bytes] = {}
    stat, fstat = os.stat, os.fstat

    def rotate() -> None:
        nonlocal rotated
        if not rotated:
            active.rename(str(active) + ".1")
            active.write_bytes(new)
            rotated = True

    def inspect_path(path: str, **kwargs: object) -> os.stat_result:
        result = stat(path, **kwargs)
        if rotation == "inspection" and str(path) == str(active):
            rotate()
        return result

    def inspect_fd(fd: int) -> os.stat_result:
        result = fstat(fd)
        if rotation == "inspection" and result.st_ino == old_ino:
            rotate()
        return result

    def put(key: str, body: bytes) -> None:
        inode = int(key.rsplit("/", 1)[1].split("-", 1)[0])
        uploaded[inode] = uploaded.get(inode, b"") + body
        if rotation == "upload":
            rotate()

    monkeypatch.setattr(os, "stat", inspect_path)
    monkeypatch.setattr(os, "fstat", inspect_fd)
    monkeypatch.setattr(shipper, "put", put)
    position: dict[str, int] = {}
    shipper.ship("test-host", active.name, position)
    shipper.ship("test-host", active.name, position)
    assert uploaded == {old_ino: old, stat(active).st_ino: new}
    assert position == {"ino": stat(active).st_ino, "offset": len(new)}
