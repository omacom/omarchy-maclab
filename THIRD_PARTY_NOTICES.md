# Third-party notices

Omarchy Mac Lab's own code is MIT licensed (see LICENSE). It began as
[maclab](https://github.com/chriskearney/maclab) by Chris Kearney and as
omarchy-m-testing; both are MIT and their histories are kept in this repository.

## The lab's interface

The interface under `internal/web/static` bundles these third-party files, which keep their own licenses.

| Files | What | License |
|---|---|---|
| `JetBrainsMono-*.woff2` | [JetBrains Mono](https://github.com/JetBrains/JetBrainsMono) | SIL OFL 1.1, see `JetBrainsMono-OFL.txt` beside them |
| `Inter-*.woff2` | [Inter](https://github.com/rsms/inter) 4.1 | SIL OFL 1.1, see `Inter-OFL.txt` beside them |
| `omarchy-logo.svg`, `icon.png`, `icon.svg` | The Omarchy logo from [Omarchy](https://github.com/basecamp/omarchy) | MIT, below |

## The test tool and the Rails reference

| Files | What | License |
|---|---|---|
| `cli/omarchy_m_test/vendor/omarchy-mac/` | The check scripts of [omarchy-mac](https://github.com/omacom/omarchy-mac), unmodified; commit and hashes in `ORIGIN` | MIT, Copyright (c) David Heinemeier Hansson, see `LICENSE` beside them |
| `cli/omarchy_m_test/gl_spec/glprobe.py`, the tables in `cli/omarchy_m_test/gpu_spec.py`, the Neural Engine probe in `engines.py`, the discovery in `hwread.py`, `catalogue/macos-reference/j616s.json` | From [aurora-diag](https://github.com/aurora-silicon/aurora-diag): its probe, tables and macOS baseline | MIT, Copyright (c) 2026 Aurora Silicon, see `cli/omarchy_m_test/gl_spec/LICENSE-aurora-diag` |
| `site/app/assets/fonts/JetBrainsMono-*.woff2` | [JetBrains Mono](https://github.com/JetBrains/JetBrainsMono) | SIL OFL 1.1, see `JetBrainsMono-OFL.txt` beside them |

## The benchmark programs

A Lab release carries benchmark programs built from the sources `release/bench-pins` pins, unmodified (`release/bench.sh`). Each archive, `bench-PROGRAM-SYSTEM-arm64.tar.gz`, holds its source's licence beside the program, and its `BUILD.json` names the source's commit or file and checksum.

| Program | Source | License |
|---|---|---|
| `coremark` | [CoreMark](https://github.com/eembc/coremark) by EEMBC | Apache 2.0, and EEMBC's acceptable use agreement for the CoreMark name, see `LICENSE.md` beside it |
| `stream` | [STREAM](https://www.cs.virginia.edu/stream/) 5.10 by John D. McCalpin | STREAM's own licence, see `LICENSE.txt` beside it |
| `clpeak` | [clpeak](https://github.com/krrishnarraj/clpeak), with Khronos's [OpenCL headers](https://github.com/KhronosGroup/OpenCL-Headers) and [C++ bindings](https://github.com/KhronosGroup/OpenCL-CLHPP) | GPL 3.0, see `LICENSE` beside it; the source is the pinned commit |
| `fio` | [fio](https://github.com/axboe/fio) by Jens Axboe | GPL 2.0, see `COPYING` and `MORAL-LICENSE` beside it; the source is the pinned commit |
| `mlx-bench` | the lab's own, linked with [MLX](https://github.com/ml-explore/mlx) and its metal-cpp, nlohmann/json and fmt | MIT (the lab's and MLX's, beside it) |

`ane-bench` and `smc-power` are the lab's own (`tools/bench-macos/`), MIT.

Omarchy Mac Lab isn't an official Omarchy project. The Omarchy name and logo belong to
their owners and are used here to match the desktop the lab's Macs run.

## Omarchy license

```
Copyright (c) David Heinemeier Hansson

Permission is hereby granted, free of charge, to any person obtaining
a copy of this software and associated documentation files (the
"Software"), to deal in the Software without restriction, including
without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to
permit persons to whom the Software is furnished to do so, subject to
the following conditions:

The above copyright notice and this permission notice shall be
included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
```
