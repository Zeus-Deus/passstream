#!/usr/bin/env python3
"""Read the sibling PassPage source/dependencies into an isolated build fixture."""
from pathlib import Path
import json
import shutil

source = Path(__file__).resolve().parents[2] / "passpage"
dest = Path(__file__).resolve().parents[1] / "build/passpage-local-source"
dest.mkdir(parents=True, exist_ok=True)
shutil.copytree(source / "backend/src", dest / "src", dirs_exist_ok=True)
(dest / "package.json").write_text('{"type":"module"}\n')
modules = dest / "node_modules"
modules.mkdir(exist_ok=True)
packages = json.loads((source / "backend/package.json").read_text())["dependencies"]
packages.pop("@passpage/api-types")
for package in packages:
    candidates = sorted((source / "node_modules/.pnpm").glob(package.replace("/", "+") + "@*/node_modules/" + package))
    if not candidates:
        raise SystemExit("Missing local dependency: " + package)
    target = modules / package
    target.parent.mkdir(parents=True, exist_ok=True)
    if not target.is_symlink():
        target.symlink_to(candidates[0])
api_types = dest / "api-types"
shutil.copytree(source / "packages/api-types", api_types, dirs_exist_ok=True, ignore=shutil.ignore_patterns("node_modules"))
(api_types / "node_modules").mkdir(exist_ok=True)
zod = sorted((source / "node_modules/.pnpm").glob("zod@3*/node_modules/zod"))[0]
if not (api_types / "node_modules/zod").is_symlink():
    (api_types / "node_modules/zod").symlink_to(zod)
api = modules / "@passpage/api-types"
api.parent.mkdir(exist_ok=True)
if not api.is_symlink():
    api.symlink_to(api_types)
print(dest)
