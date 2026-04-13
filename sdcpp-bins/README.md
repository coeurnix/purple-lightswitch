# sdcpp-bins

`download_release_bins.py` downloads the latest `stable-diffusion.cpp` release binaries into the platform subdirectories in this folder.

Flags:

- `--all`: download binaries for all supported target directories instead of only the current system's default target.
- `--target <name>`: download only one target directory. Valid values are `win-avx2-x64`, `win-rocm-x64`, `win-cuda-x64`, `linux-vulkan-x64`, and `osx-arm64`.
- `--dry-run`: show which assets would be downloaded and extracted without changing any files.

Without `--all` or `--target`, the script auto-selects the host platform. On Windows it prefers CUDA for NVIDIA GPUs, ROCm for AMD GPUs, and otherwise falls back to the AVX2 build.
