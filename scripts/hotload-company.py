"""Install an already-built plugin on an existing Linux CPA host without restarting it."""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import time

import yaml


PLUGIN = "cpa-codex-candy-eval"
REPOSITORY = "https://github.com/qinghua362330/cpa-plugin-codex-candy-eval"


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def container_state(name):
    result = subprocess.check_output(["docker", "inspect", name, "--format", "{{json .State}}"], text=True)
    state = json.loads(result)
    if not state["Running"]:
        raise RuntimeError("CPA is not running")
    return state["StartedAt"]


def store_nodes(text):
    node = yaml.compose(text)
    for part in ("plugins", "configs", PLUGIN, "store"):
        matches = [v for k, v in node.value if k.value == part]
        if len(matches) != 1:
            raise RuntimeError("missing or ambiguous store configuration")
        node = matches[0]
    return {k.value: v for k, v in node.value}


def patch_store(path, edits, expected):
    original = path.read_text()
    current = yaml.safe_load(original)["plugins"]["configs"][PLUGIN]["store"]
    if any(current.get(k) != v for k, v in expected.items()):
        raise RuntimeError("store configuration changed concurrently; refusing to overwrite it")
    nodes = store_nodes(original)
    updated = original
    replacements = []
    for key, value in edits.items():
        node = nodes[key]
        if not isinstance(node, yaml.ScalarNode):
            raise RuntimeError("store value is not a scalar")
        replacements.append((node.start_mark.index, node.end_mark.index, json.dumps(value)))
    for start, end, value in sorted(replacements, reverse=True):
        updated = updated[:start] + value + updated[end:]
    parsed = yaml.safe_load(updated)
    if any(parsed["plugins"]["configs"][PLUGIN]["store"][k] != v for k, v in edits.items()):
        raise RuntimeError("configuration verification failed")
    # This is a single-file Docker bind mount: preserve the inode so CPA sees the change.
    with path.open("r+b") as stream:
        if stream.read().decode() != original:
            raise RuntimeError("configuration changed concurrently")
        stream.seek(0)
        stream.write(updated.encode())
        stream.truncate()
        stream.flush()
        os.fsync(stream.fileno())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("root", type=Path)
    parser.add_argument("upload", type=Path)
    parser.add_argument("version")
    parser.add_argument("sha256")
    parser.add_argument("--container", default="cli-proxy-api-official")
    args = parser.parse_args()
    if not args.version or any(c not in "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ.-" for c in args.version):
        raise RuntimeError("invalid version")
    if digest(args.upload) != args.sha256:
        raise RuntimeError("artifact checksum mismatch")
    started = container_state(args.container)
    config = args.root / "config.yaml"
    cfg = yaml.safe_load(config.read_text())
    plugin = cfg["plugins"]["configs"][PLUGIN]
    if not cfg["plugins"]["enabled"] or not plugin["enabled"]:
        raise RuntimeError("plugin must already be enabled")
    old_store = plugin["store"]
    directory = args.root / "plugins/linux/amd64"
    old_library = directory / f"{PLUGIN}-v{old_store['version']}.so"
    if not old_library.is_file():
        raise RuntimeError("current pinned library is missing")
    destination = directory / f"{PLUGIN}-v{args.version}.so"
    if destination.exists():
        raise RuntimeError("target version already exists; do not overwrite a loaded library")
    backup = args.root / "backups" / ("candy-company-" + datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ"))
    backup.mkdir(mode=0o700, parents=True, exist_ok=False)
    shutil.copy2(config, backup / "config.yaml")
    os.chmod(backup / "config.yaml", 0o600)
    shutil.copy2(old_library, backup / old_library.name)
    for state in (args.root / "plugins").glob(f"{PLUGIN}-state*.json*"):
        shutil.copy2(state, backup / state.name)
        os.chmod(backup / state.name, 0o600)
    edits = {
        "version": args.version, "release-tag": "v" + args.version,
        "repository": REPOSITORY, "homepage": REPOSITORY,
        "source-id": "company", "source-name": "Company",
        "source-url": "https://raw.githubusercontent.com/qinghua362330/cpa-plugin-codex-candy-eval/main/registry.company.json",
    }
    originals = {k: old_store[k] for k in edits}
    log = args.root / "logs/main.log"
    offset = log.stat().st_size if log.exists() else 0
    manifest = {"version": args.version, "sha256": args.sha256, "old_version": old_store["version"], "container_started_at": started}
    (backup / "manifest.json").write_text(json.dumps(manifest, indent=2))
    print(json.dumps({"backup": str(backup), "phase": "prepared"}), flush=True)
    shutil.copy2(args.upload, directory / f".{destination.name}.upload")
    staged = directory / f".{destination.name}.upload"
    if digest(staged) != args.sha256:
        raise RuntimeError("staged artifact checksum mismatch")
    os.chmod(staged, 0o644)
    os.replace(staged, destination)
    applied = False
    try:
        patch_store(config, edits, originals)
        applied = True
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            if container_state(args.container) != started:
                raise RuntimeError("CPA restarted during hotload")
            with log.open() as stream:
                if log.stat().st_size >= offset:
                    stream.seek(offset)
                lines = stream.read()
            evidence = [line for line in lines.splitlines() if PLUGIN in line and
                        f"active_version={args.version}" in line and "plugin hot reloaded" in line]
            if evidence:
                manifest["verified"] = "hot-reloaded; container unchanged"
                (backup / "manifest.json").write_text(json.dumps(manifest, indent=2))
                print(json.dumps({**manifest, "backup": str(backup)}), flush=True)
                return
            time.sleep(2)
        raise RuntimeError("no confirmed hotload event within 90 seconds")
    except Exception:
        if applied:
            patch_store(config, originals, edits)
        if destination.exists():
            os.replace(destination, backup / (destination.name + ".failed"))
        print(json.dumps({"rollback_requested": True, "backup": str(backup)}), flush=True)
        raise


if __name__ == "__main__":
    main()
