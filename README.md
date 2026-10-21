# dbuild

Distributed compile launcher for CMake/Ninja C and C++ builds using clang (GNU-style `clang`/`clang++`,
Android NDK, and `clang-cl` cross builds). A local daemon decides, job by job, whether a compile
runs on this machine or on a helper reached over ssh. It sends a job remotely only when the helper
is expected to return the object sooner than the local queue would.

- Helpers need nothing preinstalled except ssh and a matching compiler. The daemon deploys the
  `dbuild` helper binary itself.
- Inputs are mirrored on the helper by content hash, so only changed headers and sources are uploaded.
- Every remote result is checked: the helper reports the hash of every file the compile read, and
  any mismatch with the local copy falls back to a local compile.
- Precompiled headers (`-include-pch`, `-Xclang -emit-pch`, MSVC-style `/Yc` + `/Yu`) are rebuilt
  on each helper, so the TUs that use them can be distributed.
- Anything dbuild doesn't understand (preprocess-only, modules, `-save-temps`, non-clang tools, ...)
  runs locally, unchanged.

## Building

```sh
./build.sh
```

This produces `bin/dbuild` for this machine, `bin/dbuild-{darwin,linux}-{arm64,amd64}` (the binaries
deployed to helpers), and `bin/dbuild-ninja` (a symlink used as the Ninja wrapper).

## Quick start

```sh
bin/dbuild init          # writes ~/.config/dbuild/config.toml; add your helpers there
cmake -S . -B build -G Ninja \
  -DCMAKE_C_COMPILER_LAUNCHER=$PWD/bin/dbuild \
  -DCMAKE_CXX_COMPILER_LAUNCHER=$PWD/bin/dbuild \
  -DCMAKE_PROJECT_INCLUDE=$PWD/cmake/dbuild.cmake
ninja -C build -j 64
```

The daemon starts automatically on the first compile and exits after 2 hours idle.

- **Use a high `-j`.** Ninja has to hand out enough jobs for the helpers to get any; the daemon does
  the real throttling.
- **Pass `CMAKE_PROJECT_INCLUDE`** for any project that caps compiles with a Ninja job pool
  (`CMAKE_JOB_POOL_COMPILE`). It raises the pool to 256 (override with `-DDBUILD_POOL_DEPTH=N`).
  Without it a capped project keeps the local slots unsaturated, and nothing is ever distributed.

### IDEs

| Command | Effect |
|---|---|
| `dbuild install` | Both of the below. |
| `dbuild install env` | Sets `CMAKE_{C,CXX}_COMPILER_LAUNCHER` for GUI apps (a LaunchAgent on macOS, `environment.d` on Linux). Applies to newly configured build dirs. |
| `dbuild install gradle` | Adds a Gradle init script so Android Gradle Plugin CMake builds use the launcher, the `dbuild-ninja` wrapper and the CMake include. Disable for one build with `-Pdbuild.enabled=false`. |
| `dbuild uninstall` | Undoes both. |

### Commands

| Command | |
|---|---|
| `dbuild <compiler> <args...>` | Compiler launcher (what CMake runs). |
| `dbuild status` | Daemon state: local slots, helpers, replicated PCHs, current build. |
| `dbuild stop` | Stop the daemon once it is idle. |
| `dbuild init` | Write the default global config if there is none. |
| `dbuild repo-init` | Create an untracked per-repo config (see below) and add it to `.git/info/exclude`. |
| `dbuild version` | Print the build version. |
| `dbuild daemon`, `dbuild serve` | Internal: the scheduler, and the helper side (started over ssh). |

## Configuration

There are two TOML files. The per-repo file is merged over the global one for compiles in that repo.
The repo is found from the build dir's `CMakeCache.txt` (`CMAKE_HOME_DIRECTORY`), then by walking up
to the nearest `.git`.

### Global: `~/.config/dbuild/config.toml`

Override the location with `DBUILD_CONFIG`.

```toml
[scheduler]
local_slots = 10            # parallel local compiles (default: CPU count)
remote_margin_ms = 150      # remote must beat local by this much to be chosen
ninja_jobs = 200            # -j the dbuild-ninja wrapper adds when the caller passed none
override_jobs = false       # wrapper also replaces a caller's -j
ninja = "/opt/homebrew/bin/ninja"   # real ninja behind the wrapper (default: searched on PATH etc.)

[[helpers]]
name = "fedora-2"           # label in logs/status (default: the ssh value)
ssh = "fedora-2"            # ssh destination
ssh_options = ["-p", "2222"]
slots = 24                  # parallel compiles on the helper (default: its CPU count)
speed = 1.15                # initial guess: helper job time / local job time (then learned)
download_mbps = 18          # initial guess helper -> here (then learned)
upload_mbps = 18
toolchains = ["/home/user/toolchains/LLVM-*"]   # extra toolchain roots on the helper (globs)
enabled = true

[policy.default]
mode = "distribute"         # "distribute" | "local"

[policy.Debug]
mode_full_debug = "local"   # mode when objects carry full debug info
```

### Per repo: `<repo>/.dbuild.local.toml`

Create it with `dbuild repo-init`. The file is untracked, and only the settings below take effect
per repo; set the others globally.

```toml
[scheduler]
ninja_jobs = 200
override_jobs = true        # e.g. for scripts that hardcode --parallel 8

# Sources that always compile locally. Globs are relative to the repo root (or absolute) and are
# matched against the source file's path: * and ? stay within one path component, ** spans any
# number of them.
local_sources = [
  "**/src/generated/FolderData.cpp",
  "src/assets/*.cpp",
]

[policy.default]
mode = "distribute"

[policy.Release]
mode = "distribute"

[policy.Debug]
mode_full_debug = "local"
```

Repo values replace global ones key by key; `local_sources` replaces the global list entirely.
`[policy.*]` tables are merged per key.

`local_slots`, `remote_margin_ms`, `ninja` and `[[helpers]]` are daemon-wide and are read only from
the global file.

### Policy

The policy table is chosen by the build dir's `CMAKE_BUILD_TYPE`, matched exactly first, then
case-insensitively, and falling back to `[policy.default]`. `mode` applies to every compile.
`mode_full_debug`, when set, applies instead to compiles with full debug info: `-g`, `-g2`, `-g3`,
`-gdwarf*`, and for clang-cl `/Zi`, `/Z7`. When nothing is set, the mode is `distribute`.

Full debug info makes objects several times larger, so on a slow link distributing it can lose to
compiling locally.

## Helpers

### Matching compilers

A helper can run a job when it has a clang whose `--version` identity matches the local one.
Differences that don't affect codegen are ignored:

- NDK `+pgo`/`+bolt`/`+lto` build flags
- Homebrew's `Homebrew ` prefix
- the upstream release's repo hash, so Homebrew LLVM 23.1.2 matches the official
  `LLVM-23.1.2-Linux-X64` tarball

Searched on the helper:

- `~/Android/Sdk/ndk/*` and `~/Library/Android/sdk/ndk/*`
- `$ANDROID_HOME`, `$ANDROID_SDK_ROOT` and `$ANDROID_NDK_HOME`
- every glob in the helper's `toolchains` setting

### Sandbox mode (recommended)

On Linux helpers with working `bwrap`, each compile runs in a namespace where your paths exist
verbatim. The mirrored files appear at their original absolute paths, and the helper's toolchain is
mounted at your toolchain's path. Absolute includes, `__FILE__` and debug info therefore match a
local build.

Without bwrap, the helper rewrites paths instead. That works for GNU-style clang only: commands that
`#include` absolute paths fall back to local, and clang-cl jobs never go to such a helper.

### Case-insensitive clients

macOS treats `DirectXMath.h` and `directxmath.h` as the same file, while the helper's filesystem
doesn't. dbuild uploads each file under its on-disk spelling and makes other spellings symlinks to
it, so `#pragma once` keeps working. If a compile on the helper fails with "file not found" for a
spelling it hasn't seen yet, the helper links it to the matching file and retries.

## clang-cl cross builds

`clang-cl` is detected from the compiler name or `--driver-mode=cl`. CMake's depfile flags
(`-clang:-MD -clang:-MF<file>`), `/Fo`, sources after `--`, and MSVC-style PCHs (`/Yc`, `/Yu`, `/Fp`)
are understood. The Windows SDK headers (for example from `/imsvc` dirs) are mirrored like any other
input.

For a macOS host with Homebrew LLVM, install the matching upstream LLVM release on the helper and
point `toolchains` at it:

```sh
ssh helper 'mkdir -p ~/toolchains && cd ~/toolchains &&
  curl -fL https://github.com/llvm/llvm-project/releases/download/llvmorg-23.1.2/LLVM-23.1.2-Linux-X64.tar.xz | tar xJ'
```

## Environment

| Variable | |
|---|---|
| `DBUILD_DISABLE=1` | Bypass dbuild; run the compiler directly. |
| `DBUILD_CONFIG` | Global config path. |
| `DBUILD_CACHE_DIR` | Daemon state dir (default `~/.cache/dbuild`). |
| `DBUILD_NINJA` | Real ninja used by `dbuild-ninja`. |

## Files and troubleshooting

All in `~/.cache/dbuild`:

| File | |
|---|---|
| `daemon.log` | Helper connections, uploads, PCH replication, remote failures and fallbacks, and a `build finished` summary per build (local/remote counts, fallbacks, verify failures). |
| `trace.jsonl` | One line per job: where it ran, estimated and actual time. Truncated when the daemon starts. |
| `history.gob` | Learned timings, object sizes and input lists. |

On the helper, `~/.cache/dbuild/mirror/<client>/` holds the mirrored files; deleting it only costs
a re-upload.

What to look for in `daemon.log`:

- `remote 0` in `build finished` usually means local slots were never saturated: the `-j` is too
  low, or a job pool is in the way (pass `CMAKE_PROJECT_INCLUDE`).
- `toolchain: no compiler matching ...` means the helper has no matching compiler.
- Files that fail remotely twice stay local for an hour.
