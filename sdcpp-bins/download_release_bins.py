#!/usr/bin/env python3
from __future__ import annotations

import argparse
import fnmatch
import json
import platform
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from zipfile import ZipFile


LATEST_RELEASE_URL = "https://github.com/leejet/stable-diffusion.cpp/releases/latest"
RELEASE_BY_TAG_API = (
    "https://api.github.com/repos/leejet/stable-diffusion.cpp/releases/tags/{tag}"
)
USER_AGENT = "purple-lightswitch-sdcpp-bin-downloader"

TARGETS = {
    "win-avx2-x64": "sd-master-*-bin-win-avx2-x64.zip",
    "win-rocm-x64": "sd-master-*-bin-win-rocm-x64.zip",
    "win-cuda-x64": "sd-master-*-bin-win-cuda12-x64.zip",
    "linux-vulkan-x64": "sd-master-*-bin-Linux-Ubuntu-*-x86_64-vulkan.zip",
    "osx-arm64": "sd-master-*-bin-Darwin-macOS-*-arm64.zip",
}
ALL_TARGET_SUBDIRS = (
    "win-avx2-x64",
    "win-rocm-x64",
    "win-cuda-x64",
    "linux-vulkan-x64",
    "osx-arm64",
)
CUDA_RUNTIME_SUBDIR = "win-cuda-x64"
CUDA_RUNTIME_PATTERN = "cudart-sd-bin-win-cu12-x64.zip"
CUDA_DLL_PATTERNS = ("cublas*.dll", "cuda*.dll")
DOWNLOAD_CHUNK_SIZE = 1024 * 1024
EXTRACT_CHUNK_SIZE = 1024 * 1024


def format_bytes(num_bytes: int) -> str:
    value = float(num_bytes)
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if value < 1024.0 or unit == "TB":
            if unit == "B":
                return f"{int(value)} {unit}"
            return f"{value:.1f} {unit}"
        value /= 1024.0
    return f"{int(num_bytes)} B"


class ConsoleProgress:
    def __init__(self, label: str, total: int) -> None:
        self.label = label
        self.total = max(total, 0)
        self.current = 0
        self.last_render_length = 0
        self.last_render_time = 0.0
        self.started_at = time.monotonic()
        self.finished = False
        self.interactive = sys.stdout.isatty()

    def update(self, amount: int) -> None:
        self.current += amount
        if not self.interactive:
            if self.total > 0 and self.current >= self.total:
                self.finished = True
            return
        now = time.monotonic()
        should_render = self.current >= self.total or now - self.last_render_time >= 0.1
        if should_render:
            self.render()
            self.last_render_time = now

    def render(self) -> None:
        if self.total > 0:
            width = shutil.get_terminal_size((100, 20)).columns
            bar_width = max(12, min(32, width - 48))
            ratio = min(1.0, self.current / self.total)
            filled = int(bar_width * ratio)
            bar = "#" * filled + "-" * (bar_width - filled)
            percent = int(ratio * 100)
            status = f"{format_bytes(self.current)}/{format_bytes(self.total)}"
            prefix = f"{self.label}: [{bar}] {percent:3d}% {status}"
        else:
            spinner = "|/-\\"[int(time.monotonic() * 10) % 4]
            prefix = f"{self.label}: [{spinner}] {format_bytes(self.current)}"

        elapsed = max(time.monotonic() - self.started_at, 0.001)
        speed = format_bytes(int(self.current / elapsed)) + "/s"
        line = f"{prefix} {speed}"
        padded = line.ljust(self.last_render_length)
        sys.stdout.write("\r" + padded)
        sys.stdout.flush()
        self.last_render_length = len(padded)
        if self.total > 0 and self.current >= self.total:
            self.finished = True

    def finish(self) -> None:
        elapsed = max(time.monotonic() - self.started_at, 0.001)
        speed = format_bytes(int(self.current / elapsed)) + "/s"
        if self.interactive:
            if not self.finished:
                self.render()
            sys.stdout.write("\n")
            sys.stdout.flush()
        else:
            if self.total > 0:
                print(
                    f"{self.label}: completed {format_bytes(self.current)}/"
                    f"{format_bytes(self.total)} at {speed}"
                )
            else:
                print(f"{self.label}: completed {format_bytes(self.current)} at {speed}")
        self.finished = True


def build_request(url: str, accept: str | None = None) -> urllib.request.Request:
    headers = {"User-Agent": USER_AGENT}
    if accept:
        headers["Accept"] = accept
    return urllib.request.Request(url, headers=headers)


def resolve_latest_tag() -> tuple[str, str]:
    with urllib.request.urlopen(build_request(LATEST_RELEASE_URL)) as response:
        resolved_url = response.geturl()

    marker = "/releases/tag/"
    if marker not in resolved_url:
        raise RuntimeError(f"Could not determine release tag from {resolved_url!r}.")

    tag = urllib.parse.unquote(resolved_url.split(marker, 1)[1].split("?", 1)[0])
    return tag, resolved_url


def fetch_release_assets(tag: str) -> list[dict]:
    api_url = RELEASE_BY_TAG_API.format(tag=urllib.parse.quote(tag, safe=""))
    with urllib.request.urlopen(
        build_request(api_url, accept="application/vnd.github+json")
    ) as response:
        payload = json.load(response)

    assets = payload.get("assets")
    if not isinstance(assets, list):
        raise RuntimeError(f"GitHub API response for tag {tag!r} did not include assets.")

    return assets


def is_directory_empty(path: Path) -> bool:
    path.mkdir(parents=True, exist_ok=True)
    return next(path.iterdir(), None) is None


def has_cuda_runtime_dlls(path: Path) -> bool:
    found = {pattern: False for pattern in CUDA_DLL_PATTERNS}

    for file_path in path.rglob("*"):
        if not file_path.is_file():
            continue

        name = file_path.name.lower()
        for pattern in CUDA_DLL_PATTERNS:
            if fnmatch.fnmatchcase(name, pattern):
                found[pattern] = True

        if all(found.values()):
            return True

    return False


def find_first_asset(assets: list[dict], pattern: str) -> dict | None:
    for asset in assets:
        name = asset.get("name")
        if isinstance(name, str) and fnmatch.fnmatchcase(name, pattern):
            return asset
    return None


def download_asset(asset: dict, download_dir: Path, dry_run: bool) -> Path:
    name = asset["name"]
    download_url = asset["browser_download_url"]
    zip_path = download_dir / name

    if dry_run:
        print(f"[dry-run] Would download {name} from {download_url}")
        return zip_path

    print(f"Downloading {name}...")
    with urllib.request.urlopen(build_request(download_url)) as response, zip_path.open("wb") as output_file:
        total_bytes = int(response.headers.get("Content-Length", "0") or "0")
        progress = ConsoleProgress(f"  download {name}", total_bytes)
        while True:
            chunk = response.read(DOWNLOAD_CHUNK_SIZE)
            if not chunk:
                break
            output_file.write(chunk)
            progress.update(len(chunk))
        progress.finish()

    return zip_path


def extract_zip(zip_path: Path, destination: Path, dry_run: bool) -> None:
    if dry_run:
        print(f"[dry-run] Would extract {zip_path.name} into {destination}")
        return

    destination.mkdir(parents=True, exist_ok=True)
    destination_root = destination.resolve()

    with ZipFile(zip_path) as archive:
        total_bytes = sum(member.file_size for member in archive.infolist() if not member.is_dir())
        progress = ConsoleProgress(f"  extract  {zip_path.name}", total_bytes)
        for member in archive.infolist():
            member_path = destination / member.filename
            resolved_member = member_path.resolve()

            if resolved_member != destination_root and destination_root not in resolved_member.parents:
                raise RuntimeError(
                    f"Refusing to extract {member.filename!r} outside {destination}."
                )

            if member.is_dir():
                member_path.mkdir(parents=True, exist_ok=True)
                continue

            member_path.parent.mkdir(parents=True, exist_ok=True)
            with archive.open(member) as source_file, member_path.open("wb") as output_file:
                while True:
                    chunk = source_file.read(EXTRACT_CHUNK_SIZE)
                    if not chunk:
                        break
                    output_file.write(chunk)
                    progress.update(len(chunk))

            permissions = (member.external_attr >> 16) & 0o777
            if permissions:
                try:
                    member_path.chmod(permissions)
                except OSError:
                    pass
        progress.finish()


def install_asset(asset: dict, destination: Path, dry_run: bool) -> None:
    with tempfile.TemporaryDirectory(prefix="sdcpp-bin-") as temp_dir:
        zip_path = download_asset(asset, Path(temp_dir), dry_run=dry_run)
        extract_zip(zip_path, destination, dry_run=dry_run)


def process_target(assets: list[dict], subdir: str, dry_run: bool) -> None:
    destination = Path(__file__).resolve().parent / subdir
    pattern = TARGETS[subdir]

    if not is_directory_empty(destination):
        print(f"Skipping {subdir}: directory is not empty.")
        return

    asset = find_first_asset(assets, pattern)
    if asset is None:
        raise RuntimeError(f"No asset matched {pattern!r} for {subdir}.")

    print(f"Installing {asset['name']} into {subdir}")
    install_asset(asset, destination, dry_run=dry_run)


def install_cuda_runtime_if_needed(assets: list[dict], dry_run: bool) -> None:
    destination = Path(__file__).resolve().parent / CUDA_RUNTIME_SUBDIR
    destination.mkdir(parents=True, exist_ok=True)

    if has_cuda_runtime_dlls(destination):
        print(
            f"Skipping {CUDA_RUNTIME_PATTERN}: found existing cublas* and cuda* DLLs in "
            f"{CUDA_RUNTIME_SUBDIR}."
        )
        return

    asset = find_first_asset(assets, CUDA_RUNTIME_PATTERN)
    if asset is None:
        raise RuntimeError(f"No asset matched {CUDA_RUNTIME_PATTERN!r}.")

    print(f"Installing {asset['name']} into {CUDA_RUNTIME_SUBDIR}")
    install_asset(asset, destination, dry_run=dry_run)


def detect_windows_gpu_kind() -> str:
    commands = (
        [
            "powershell",
            "-NoProfile",
            "-Command",
            (
                "Get-CimInstance Win32_VideoController | "
                "Select-Object Name,AdapterCompatibility | ConvertTo-Json -Compress"
            ),
        ],
        [
            "pwsh",
            "-NoProfile",
            "-Command",
            (
                "Get-CimInstance Win32_VideoController | "
                "Select-Object Name,AdapterCompatibility | ConvertTo-Json -Compress"
            ),
        ],
    )

    adapters: list[dict] = []
    for command in commands:
        try:
            result = subprocess.run(
                command,
                capture_output=True,
                check=True,
                text=True,
                encoding="utf-8",
            )
        except (FileNotFoundError, subprocess.CalledProcessError):
            continue

        output = result.stdout.strip()
        if not output:
            continue

        try:
            data = json.loads(output)
        except json.JSONDecodeError:
            continue

        if isinstance(data, dict):
            adapters = [data]
        elif isinstance(data, list):
            adapters = [item for item in data if isinstance(item, dict)]
        break

    gpu_strings = []
    for adapter in adapters:
        for key in ("Name", "AdapterCompatibility"):
            value = adapter.get(key)
            if isinstance(value, str):
                gpu_strings.append(value.lower())

    combined = " ".join(gpu_strings)
    if "nvidia" in combined:
        return "nvidia"
    if "advanced micro devices" in combined or "amd" in combined or "radeon" in combined:
        return "amd"
    return "other"


def detect_default_targets() -> tuple[list[str], str]:
    system = platform.system()
    if system == "Windows":
        gpu_kind = detect_windows_gpu_kind()
        if gpu_kind == "nvidia":
            return ["win-cuda-x64"], "Windows with NVIDIA GPU"
        if gpu_kind == "amd":
            return ["win-rocm-x64"], "Windows with AMD GPU"
        return ["win-avx2-x64"], "Windows with no NVIDIA or AMD GPU detected"
    if system == "Darwin":
        return ["osx-arm64"], "macOS host"
    if system == "Linux":
        return ["linux-vulkan-x64"], "Linux host"

    raise RuntimeError(f"Unsupported operating system {system!r}. Use --all if needed.")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Download and extract the latest stable-diffusion.cpp release binaries "
            "into the sibling sdcpp-bins directories."
        )
    )
    target_group = parser.add_mutually_exclusive_group()
    target_group.add_argument(
        "--all",
        action="store_true",
        help="Download binaries for all supported target directories.",
    )
    target_group.add_argument(
        "--target",
        choices=ALL_TARGET_SUBDIRS,
        help="Download only the specified target directory.",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Show what would be downloaded and extracted without making changes.",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()

    try:
        tag, resolved_url = resolve_latest_tag()
        assets = fetch_release_assets(tag)
    except urllib.error.HTTPError as error:
        print(f"GitHub request failed: {error}", file=sys.stderr)
        return 1
    except urllib.error.URLError as error:
        print(f"Network error while contacting GitHub: {error}", file=sys.stderr)
        return 1
    except RuntimeError as error:
        print(error, file=sys.stderr)
        return 1

    print(f"Resolved latest release: {tag}")
    print(f"Release URL: {resolved_url}")

    try:
        if args.all:
            target_subdirs = list(ALL_TARGET_SUBDIRS)
            print("Selected targets: all supported platforms")
        elif args.target:
            target_subdirs = [args.target]
            print(f"Selected targets: {args.target} (explicit override)")
        else:
            target_subdirs, selection_reason = detect_default_targets()
            print(f"Selected targets: {', '.join(target_subdirs)} ({selection_reason})")

        for subdir in target_subdirs:
            process_target(assets, subdir, dry_run=args.dry_run)

        if CUDA_RUNTIME_SUBDIR in target_subdirs:
            install_cuda_runtime_if_needed(assets, dry_run=args.dry_run)
    except RuntimeError as error:
        print(error, file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
