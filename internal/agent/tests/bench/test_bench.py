import copy
import hashlib
import io
import json
import os
import pathlib
import re
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import types
import unittest

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import bench  # noqa: E402

SET = json.loads((HERE / "sets" / "bench-1.json").read_text())
CPUS = 8

COREMARK = """2K performance run parameters for coremark.
CoreMark Size    : 666
Total ticks      : 12345
Iterations/Sec   : 177922.751872
[0]crcfinal      : 0xa14c
Correct operation validated. See README.md for run and reporting rules.
CoreMark 1.0 : 177922.751872 / Clang 20.1.8 -O2 / Heap / 8:PThreads
"""

COREMARK_SHORT = """Iterations/Sec   : 90000.0
ERROR! Must execute for at least 10 secs for a valid result!
[0]crcfinal      : 0xa14c
Correct operation validated. See README.md for run and reporting rules.
"""

SEVEN_ZIP = """
7-Zip 26.06 (arm64) : Copyright (c) 1999-2026 Igor Pavlov : 2026-05-03
      Compressing  |                  Decompressing
Dict     Speed Usage    R/U Rating  |      Speed Usage    R/U Rating
         KiB/s     %   MIPS   MIPS  |      KiB/s     %   MIPS   MIPS

22:      55555   1033   5246  54046  |     545263   1179   3943  46519
23:      54000   1040   5300  55020  |     540000   1180   3950  46600
----------------------------------  | ------------------------------
Avr:     55000   1036   5250  54300  |     545000   1180   3950  46600
Tot:             1108   4600  50450
"""

BOOT = """Startup finished in 4.611s (firmware) + 1.284s (loader) + 2.040s (kernel) + 3.501s (initrd) + 1min 2.103s (userspace) = 1min 13.539s
graphical.target reached after 1min 2.000s in userspace.
"""

CLPEAK_1 = """Platform: rusticl
  Device: Apple M2 Max (G14C B1)
    Driver version  : 25.0.0 (Linux ARM64)

    Global memory bandwidth (GBPS)
      float   : 300.12
      float2  : 310.5
      float4  : 305.0

    Single-precision compute (GFLOPS)
      float   : 1234.56
      float2  : 2345.67
      float4  : 3456.78
      float8  : 3400.12

    Half-precision compute (GFLOPS)
      half    : 9999.99
"""

CLPEAK_3 = """Backend: OpenCL
  Platform 0: rusticl
    Device 0: Apple M2 Max (G14C B1)
      Driver version   : 26.2.99
      Compute units    : 38
      Clock frequency  : 1000 MHz

      Single-precision compute
        float    : 9.45 TFLOPS
        float2   : 10.2 TFLOPS
        float4   : 980.5 GFLOPS

      Global memory bandwidth
        float    : 312.4 GB/s
        float16  : 330.1 GB/s

"""

CLPEAK_LLVMPIPE = """Backend: OpenCL
  Platform 0: rusticl
    Device 0: llvmpipe (LLVM 19.1.7, 128 bits)

      Single-precision compute
        float    : 90.1 GFLOPS
"""

STREAM = """-------------------------------------------------------------
STREAM version $Revision: 5.10 $
-------------------------------------------------------------
Array size = 50331648 (elements), Offset = 0 (elements)
Memory per array = 384.0 MiB (= 0.4 GiB).
-------------------------------------------------------------
Function    Best Rate MB/s  Avg time     Min time     Max time
Copy:          101234.5     0.008012     0.007955     0.008101
Scale:          98765.4     0.008201     0.008154     0.008299
Add:           104567.8     0.011623     0.011552     0.011701
Triad:         105678.9     0.011501     0.011431     0.011600
-------------------------------------------------------------
Solution Validates: avg error less than 1.000000e-13 on all three arrays
-------------------------------------------------------------
"""


def fio_json(rw, bw_bytes, iops, bw_dev=1000, iops_stddev=500):
    side = {"io_bytes": 8 << 30, "bw_bytes": bw_bytes, "bw": bw_bytes // 1024, "iops": iops,
            "bw_dev": bw_dev, "iops_stddev": iops_stddev}
    zero = {"io_bytes": 0, "bw_bytes": 0, "bw": 0, "iops": 0.0}
    job = {"jobname": rw, "groupid": 0, "error": 0,
           "read": side if "read" in rw else zero, "write": side if "write" in rw else zero}
    return json.dumps({"fio version": "fio-3.41", "jobs": [job]})


FFMPEG = ("frame=  120 fps=0.0 q=-0.0 size=N/A time=00:00:04.00\r"
          "frame=  300 fps=180 q=-0.0 Lsize=N/A time=00:00:10.00 bitrate=N/A speed=6.01x\n"
          "bench: utime=0.101s stime=0.120s rtime=1.500s\n")

LLAMA_LOG = """ggml_vulkan: Found 1 Vulkan devices:
ggml_vulkan: 0 = Apple M2 Max (G14C B1) (Honeykrisp) | uma: 1 | fp16: 1 | warp size: 32
load_tensors: offloading 28 repeating layers to GPU
load_tensors: offloading output layer to GPU
load_tensors: offloaded 29/29 layers to GPU
"""


def llama_row(p, n, ts, sd, ngl=999):
    return json.dumps({"build_commit": "abc1234", "backends": "Vulkan", "gpu_info": "Apple M2 Max",
                       "model_type": "llama 3B Q4_K - Medium", "n_gpu_layers": ngl, "n_prompt": p, "n_gen": n,
                       "avg_ts": ts, "stddev_ts": sd})


class Parsers(unittest.TestCase):
    def test_boot_total(self):
        self.assertAlmostEqual(bench.parse_boot(BOOT), 73.539)
        self.assertAlmostEqual(bench.parse_boot("Startup finished in 2.5s (kernel) + 820ms (userspace) = 3.320s\n"), 3.32)

    def test_7z_total_rating(self):
        self.assertEqual(bench.parse_7z(SEVEN_ZIP), 50450)

    def test_coremark(self):
        self.assertEqual(bench.parse_coremark(COREMARK), 177922.751872)
        with self.assertRaisesRegex(bench.ParseError, "at least 10 secs"):
            bench.parse_coremark(COREMARK_SHORT)
        bench.coremark_valid(COREMARK_SHORT)
        with self.assertRaises(bench.ParseError):
            bench.coremark_valid("ERROR! list crc 0x0000 - should be 0xe714\n")

    def test_clpeak_fp32_and_bandwidth(self):
        self.assertAlmostEqual(bench.parse_clpeak_fp32(CLPEAK_1), 3456.78)
        self.assertAlmostEqual(bench.parse_clpeak_fp32(CLPEAK_3), 10200.0)
        self.assertAlmostEqual(bench.parse_clpeak_bandwidth(CLPEAK_1), 310.5)
        self.assertAlmostEqual(bench.parse_clpeak_bandwidth(CLPEAK_3), 330.1)

    def test_stream_triad_and_copy_in_gb_s(self):
        self.assertAlmostEqual(bench.parse_stream(STREAM, "Triad"), 105.679)
        self.assertAlmostEqual(bench.parse_stream(STREAM, "Copy"), 101.234)
        with self.assertRaises(bench.ParseError):
            bench.parse_stream(STREAM.replace("Solution Validates", "Failed Validation"), "Triad")

    def test_fio_group(self):
        got = bench.parse_fio("fio: some warning\n" + fio_json("randread", 400_000_000, 97656.25), "randread")
        self.assertEqual(got["mbs"], 400.0)
        self.assertEqual(got["kiops"], 97.656)
        self.assertEqual(got["kiops_spread"], 0.5)

    def test_ffmpeg_frames_and_rate(self):
        self.assertEqual(bench.parse_ffmpeg(FFMPEG), (300, 200.0))

    def test_llama_rows(self):
        rows = bench.parse_llama(LLAMA_LOG + llama_row(512, 0, 812.5, 3.1) + "\n")
        self.assertEqual(bench.llama_row(rows, 512, 0)["avg_ts"], 812.5)
        with self.assertRaises(bench.ParseError):
            bench.llama_row(rows, 0, 128)

    def test_sizes(self):
        self.assertEqual(bench.size_bytes("8G"), 8 << 30)
        self.assertEqual(bench.size_bytes("256M"), 256 << 20)
        self.assertEqual(bench.size_bytes("12288K"), 12 << 20)

    def test_garbage_is_an_error(self):
        for fn in (bench.parse_7z, bench.parse_boot, bench.parse_clpeak_fp32, bench.parse_clpeak_bandwidth,
                   bench.parse_ffmpeg, bench.parse_llama, bench.parse_ane, bench.parse_mlx):
            with self.assertRaises(bench.ParseError):
                fn("nothing here")


class Proof(unittest.TestCase):
    """A row counts only when the run shows the work ran where it says."""

    def test_clpeak_on_the_physical_gpu(self):
        self.assertIsNone(bench.clpeak_verdict(0, CLPEAK_3))
        self.assertEqual(bench.clpeak_verdict(0, CLPEAK_LLVMPIPE), "fell_back")
        self.assertEqual(bench.clpeak_verdict(1, "Backend: OpenCL\nclGetPlatformIDs (-1001)\nno platforms found"), "no_driver")
        self.assertEqual(bench.clpeak_verdict(139, "Segmentation fault"), "error")

    def test_decode_in_hardware(self):
        self.assertIsNone(bench.decode_verdict(0, FFMPEG))
        self.assertEqual(bench.decode_verdict(0, "[h264 @ 0x1] Failed setup for format drm_prime: hwaccel initialisation returned error.\n" + FFMPEG), "fell_back")
        self.assertEqual(bench.decode_verdict(1, "Device creation failed: -19.\n"), "no_driver")
        self.assertEqual(bench.decode_verdict(1, "Invalid data found when processing input"), "error")

    def test_llama_offloads_every_layer_to_a_real_gpu(self):
        rows = [json.loads(llama_row(512, 0, 1, 0))]
        self.assertIsNone(bench.llama_gpu_verdict(LLAMA_LOG, rows))
        self.assertEqual(bench.llama_gpu_verdict(LLAMA_LOG.replace("29/29", "20/29"), rows), "fell_back")
        self.assertEqual(bench.llama_gpu_verdict(LLAMA_LOG.replace("Apple M2 Max (G14C B1) (Honeykrisp)", "llvmpipe (LLVM 19.1.7)"), rows), "fell_back")
        self.assertEqual(bench.llama_gpu_verdict("load_tensors: offloaded 0/29 layers to GPU", rows), "fell_back")

    def test_pages_drawn_by_the_gpu_at_60_hz(self):
        gpu = {"renderer": "ANGLE (Mesa, AGX G13/G14, OpenGL ES 3.2)", "dpr": 2}
        self.assertIsNone(bench.browser_verdict(gpu, 59.9))
        self.assertEqual(bench.browser_verdict(gpu, 30.0), "fell_back")
        self.assertEqual(bench.browser_verdict(gpu, 120.0), "fell_back")
        self.assertEqual(bench.browser_verdict(dict(gpu, dpr=1), 60.0), "error")
        for r in ("ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero)), SwiftShader driver)",
                  "ANGLE (Mesa, llvmpipe (LLVM 19.1.7, 128 bits), OpenGL ES 3.2)", None):
            self.assertEqual(bench.browser_verdict({"renderer": r, "dpr": 2}, 60.0), "software_renderer")


class FakeHost(bench.Host):
    """A sysfs tree in a temp dir, a clock that moves only when slept, and
    tools by name that answer without running anything."""

    def __init__(self, root):
        super().__init__(root=root)
        self.t = 0.0
        self.temps = []
        self.ran = []
        self.tools = {}
        self.vfs = types.SimpleNamespace(f_frsize=1, f_blocks=500 * 10**9, f_bavail=300 * 10**9)

    def now(self):
        return self.t

    def sleep(self, s):
        self.t += s
        if self.temps:
            self.write("sys/class/thermal/thermal_zone0/temp", str(self.temps.pop(0)))

    def write(self, rel, text):
        p = pathlib.Path(self.root, rel)
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text)

    def which(self, name):
        return f"/usr/bin/{name}" if name in self.tools else None

    def cpus(self):
        return CPUS

    def statvfs(self, path):
        return self.vfs

    def run(self, cmd, timeout=None, env=None, gui=False, cwd=None):
        cmd = [str(c) for c in cmd]
        self.ran.append((cmd, env or {}, gui))
        tool = self.tools.get(os.path.basename(cmd[0]))
        if tool is None:
            return bench.Ran(127, f"{cmd[0]}: not found", 0.0)
        r = tool(cmd)
        return r if isinstance(r, bench.Ran) else bench.Ran(0, r, 1.0)

    def argv(self, name):
        return [c for c, _, _ in self.ran if os.path.basename(c[0]) == name]


def opt(cmd, name):
    for c in cmd:
        if c.startswith(name + "="):
            return c.split("=", 1)[1]
    return None


class Case(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        root = pathlib.Path(self.d.name)
        self.cache, self.work = root / "cache", root / "work"
        self.cache.mkdir()
        self.work.mkdir()
        self.h = FakeHost(str(root / "sys-root"))
        self.set = copy.deepcopy(SET)
        self.log = io.StringIO()
        self.ctx = bench.Ctx(self.h, self.set, self.cache, self.work, log=self.log)

    def tearDown(self):
        self.d.cleanup()

    def pin(self, fid, files=None, data=None):
        """The set's file pinned at bytes put in the cache: a tar of files
        ({path: (text, mode)}) or the data itself."""
        if files is not None:
            buf = io.BytesIO()
            with tarfile.open(fileobj=buf, mode="w:gz") as t:
                for rel, (text, mode) in files.items():
                    b = text.encode()
                    info = tarfile.TarInfo(rel)
                    info.size, info.mode = len(b), mode
                    t.addfile(info, io.BytesIO(b))
            data = buf.getvalue()
        data = data if data is not None else b"pinned " + fid.encode()
        sha = hashlib.sha256(data).hexdigest()
        (self.cache / sha).write_bytes(data)
        self.ctx.files[fid]["sha256"] = sha
        return sha

    def metric(self, mid):
        return next(m for m in self.set["metrics"] if m["id"] == mid)

    def run1(self, mid, cool=None):
        [e] = bench.run_list([self.metric(mid)], self.ctx, cool=cool)
        self.assertEqual(tuple(e), bench.FIELDS)
        return e


class CPU(Case):
    def coremarks(self):
        self.pin("coremark", {"coremark-1.01/coremark": ("", 0o755), "coremark-1.01/LICENSE.md": ("", 0o644)})
        self.h.tools["coremark"] = lambda c: COREMARK

    def test_coremark_one_core(self):
        self.coremarks()
        e = self.run1("coremark-single")
        self.assertEqual((e["value"], e["unit"], e["reason"], e["version"]), (177922.751872, "iterations/s", None, "1.01"))
        self.assertEqual(self.h.argv("coremark")[0][1:], ["M1", "0x0", "0x0", "0x66", "0", "7", "1", "2000"])

    def test_coremark_all_cores_takes_a_thread_per_core(self):
        self.coremarks()
        e = self.run1("coremark-all")
        self.assertIsNone(e["reason"])
        self.assertEqual(self.h.argv("coremark")[0][1], f"M{CPUS}")

    def test_coremark_with_no_program_in_its_archive(self):
        self.pin("coremark", {"coremark-1.01/LICENSE.md": ("", 0o644)})
        self.assertEqual(self.run1("coremark-all")["reason"], "no_build")

    def test_7zip(self):
        self.pin("7zip", {"usr/bin/7z": ("", 0o755), "usr/lib/7zip/7z": ("", 0o755), "usr/lib/7zip/7z.so": ("", 0o644)})
        self.h.tools["7z"] = lambda c: SEVEN_ZIP if c[0].endswith("usr/lib/7zip/7z") and c[1:] == ["b"] else bench.Ran(1, "", 0)
        e = self.run1("7zip")
        self.assertEqual((e["value"], e["unit"], e["reason"]), (50450, "MIPS", None))

    def test_a_crash_is_an_error_and_its_text_stays_in_the_log(self):
        self.pin("7zip", {"usr/lib/7zip/7z": ("", 0o755)})
        self.h.tools["7z"] = lambda c: bench.Ran(2, "cannot open /home/alice/.cache/x", 0)
        e = self.run1("7zip")
        self.assertEqual(e["reason"], "error")
        self.assertIsNone(e["value"])
        self.assertNotIn("alice", json.dumps(e))
        self.assertIn("alice", self.log.getvalue())


class GPU(Case):
    def setUp(self):
        super().setUp()
        self.pin("clpeak", {"clpeak/bin/clpeak": ("", 0o755), "clpeak/lib/libOpenCL.so.1": ("", 0o644)})

    def test_clpeak_bandwidth_and_fp32_on_rusticl(self):
        self.h.tools["clpeak"] = lambda c: CLPEAK_3
        bw, fp = self.run1("clpeak-global-bandwidth"), self.run1("clpeak-fp32")
        self.assertEqual((bw["value"], bw["unit"], bw["api"]), (330.1, "GB/s", "Rusticl"))
        self.assertEqual((fp["value"], fp["unit"]), (10200.0, "GFLOPS"))
        (a, env, gui), (b, _, _) = [r for r in self.h.ran if r[0][0].endswith("clpeak")]
        self.assertEqual(a[1:], ["--opencl", "--global-memory-bandwidth"])
        self.assertEqual(b[1:], ["--opencl", "--single-precision-compute"])
        self.assertEqual(env["RUSTICL_ENABLE"], "asahi")
        self.assertIn("clpeak/lib", env["LD_LIBRARY_PATH"])

    def test_clpeak_on_a_software_device_fell_back(self):
        self.h.tools["clpeak"] = lambda c: CLPEAK_LLVMPIPE
        e = self.run1("clpeak-fp32")
        self.assertEqual((e["reason"], e["value"]), ("fell_back", None))


class Storage(Case):
    def setUp(self):
        super().setUp()
        self.pin("fio", {"usr/bin/fio": ("", 0o755)})
        self.set["metrics"] = [dict(m, version="3.41") for m in self.set["metrics"]]
        self.h.write("sys/block/nvme0n1/size", str(976773168))
        self.h.tools["fio"] = self.fio
        self.seen = []
        self.fail = None

    def fio(self, cmd):
        path = pathlib.Path(opt(cmd, "--filename"))
        self.seen.append((cmd, path.exists()))
        if opt(cmd, "--name") == "fill":
            path.write_bytes(b"\0" * 4096)
        elif self.fail:
            return self.fail()
        rw = opt(cmd, "--rw")
        pathlib.Path(opt(cmd, "--output")).write_text(fio_json(rw, 3_000_000_000 if "rand" not in rw else 400_000_000, 2861.0 if "rand" not in rw else 97656.25))
        return ""

    def test_sequential_read_on_a_filled_file(self):
        e = self.run1("fio-seq-read")
        self.assertEqual((e["value"], e["unit"], e["reason"]), (3000.0, "MB/s", None))
        self.assertEqual((e["capacity"], e["fill"]), (round(976773168 * 512 / 1e9), 41.7))
        (fill, _), (run, existed) = self.seen
        self.assertEqual(opt(fill, "--name"), "fill")
        self.assertEqual(opt(fill, "--size"), "8G")
        self.assertEqual(opt(fill, "--direct"), "1")
        self.assertTrue(existed)
        for k, v in {"--rw": "read", "--bs": "1M", "--size": "8G", "--direct": "1", "--numjobs": "1"}.items():
            self.assertEqual(opt(run, k), v, k)
        self.assertNotIn("--time_based", run)
        self.assertFalse(list(self.work.rglob("scratch")))

    def test_random_writes_by_32_psync_workers_in_their_own_ranges(self):
        e = self.run1("fio-rand-write-4k")
        self.assertEqual((e["value"], e["unit"], e["reason"]), (97.656, "kIOPS", None))
        run = self.seen[1][0]
        want = {"--rw": "randwrite", "--bs": "4k", "--numjobs": "32", "--iodepth": "1", "--ioengine": "psync",
                "--size": "256M", "--offset_increment": "256M", "--offset": "0", "--runtime": "30", "--direct": "1"}
        for k, v in want.items():
            self.assertEqual(opt(run, k), v, k)
        self.assertIn("--time_based", run)
        self.assertIn("--group_reporting", run)
        self.assertEqual(32 * bench.size_bytes("256M"), bench.size_bytes("8G"))

    def test_too_full_a_disk_is_not_run(self):
        self.h.vfs.f_bavail = 90 * 10**9 + (8 << 30)
        e = self.run1("fio-seq-read")
        self.assertEqual(e["reason"], "disk_too_full")
        self.assertEqual(e["capacity"], round(976773168 * 512 / 1e9))
        self.assertEqual(e["fill"], 82.0)
        self.assertFalse(self.seen)

    def test_the_file_goes_when_fio_fails(self):
        self.fail = lambda: bench.Ran(1, "fio: io_u error on file /var/tmp/x", 0)
        e = self.run1("fio-seq-write")
        self.assertEqual(e["reason"], "error")
        self.assertIsNotNone(e["capacity"])
        self.assertFalse((self.work / "fio").exists())

    def test_the_file_goes_when_the_run_is_cancelled(self):
        def cancel():
            raise bench.Cancelled()
        self.fail = cancel
        with self.assertRaises(bench.Cancelled):
            self.run1("fio-rand-read-4k")
        self.assertTrue(self.seen)
        self.assertFalse((self.work / "fio").exists())

    def test_direct_io_through_the_page_cache_fell_back(self):
        self.h.tools["fincore"] = lambda c: str(8 << 30)
        self.assertEqual(self.run1("fio-seq-read")["reason"], "fell_back")
        self.h.tools["fincore"] = lambda c: "0"
        self.assertIsNone(self.run1("fio-seq-read")["reason"])


class Memory(Case):
    def test_stream_arrays_four_times_the_largest_cache(self):
        self.pin("stream", {"stream/stream": ("", 0o755)})
        self.h.write("sys/devices/system/cpu/cpu0/cache/index2/size", "12288K")
        self.h.tools["stream"] = lambda c: STREAM
        t, c = self.run1("stream-triad"), self.run1("stream-copy")
        self.assertEqual((t["value"], t["unit"]), (105.679, "GB/s"))
        self.assertEqual(c["value"], 101.234)
        n = int(self.h.argv("stream")[0][1])
        self.assertGreaterEqual(n * 8, 4 * 96 << 20)
        self.h.write("sys/devices/system/cpu/cpu0/cache/index3/size", "262144K")
        self.assertEqual(bench.stream_elements(self.h) * 8, 4 * (256 << 20))


class Video(Case):
    def setUp(self):
        super().setUp()
        self.out = FFMPEG
        self.h.tools["ffmpeg"] = lambda c: " v4l2request\n vulkan\n" if "-hwaccels" in c else self.out
        self.h.tools["ffprobe"] = lambda c: "300\n"
        self.h.write("dev/media0", "")
        self.pin("clip-h264-1080p")

    def test_h264_decoded_in_hardware(self):
        e = self.run1("decode-h264-1080p")
        self.assertEqual((e["value"], e["unit"], e["api"], e["reason"]), (200.0, "frames/s", "V4L2 request", None))
        run = [c for c in self.h.argv("ffmpeg") if "-hwaccels" not in c][0]
        self.assertEqual(run[run.index("-hwaccel") + 1], "v4l2request")
        self.assertIn("-benchmark", run)
        self.assertNotIn("-re", run)

    def test_hevc_clip_still_a_placeholder(self):
        self.assertEqual(self.run1("decode-hevc-4k")["reason"], "placeholder_file")

    def test_software_decode_fell_back(self):
        self.out = "[h264 @ 0x1] Failed setup for format drm_prime: hwaccel initialisation returned error.\n" + FFMPEG
        self.assertEqual(self.run1("decode-h264-1080p")["reason"], "fell_back")

    def test_not_every_frame_decoded_is_an_error(self):
        self.h.tools["ffprobe"] = lambda c: "301\n"
        self.assertEqual(self.run1("decode-h264-1080p")["reason"], "error")

    def test_no_hwaccel_is_no_build_and_no_device_no_driver(self):
        self.h.tools["ffmpeg"] = lambda c: " vulkan\n" if "-hwaccels" in c else FFMPEG
        self.assertEqual(self.run1("decode-h264-1080p")["reason"], "no_build")
        self.h.tools["ffmpeg"] = lambda c: " v4l2request\n" if "-hwaccels" in c else FFMPEG
        os.unlink(pathlib.Path(self.h.root, "dev/media0"))
        self.assertEqual(self.run1("decode-h264-1080p")["reason"], "no_driver")
        del self.h.tools["ffmpeg"]
        self.assertEqual(self.run1("decode-h264-1080p")["reason"], "no_build")


class AI(Case):
    def test_neural_engine(self):
        self.pin("mobilenet-v1")
        self.assertEqual(self.run1("neural-engine-mobilenet-v1")["reason"], "no_driver")
        self.h.write("sys/class/accel/accel0/device/uevent", "DRIVER=apple-ane\nOF_NAME=ane\n")
        self.assertEqual(self.run1("neural-engine-mobilenet-v1")["reason"], "no_build")
        self.h.tools["ane-bench"] = lambda c: "device: ane\ninferences/s: 1234.5\n"
        e = self.run1("neural-engine-mobilenet-v1")
        self.assertEqual((e["value"], e["api"], e["reason"]), (1234.5, "libane", None))
        self.h.tools["ane-bench"] = lambda c: "device: cpu\ninferences/s: 80.0\n"
        self.assertEqual(self.run1("neural-engine-mobilenet-v1")["reason"], "fell_back")

    def llama(self, out):
        self.pin("llama.cpp", {"llama/bin/llama-bench": ("", 0o755), "llama/lib/libggml-vulkan.so": ("", 0o644)})
        self.pin("llama-model")
        self.h.tools["llama-bench"] = out

    def test_llama_prompt_on_the_gpu_offloads_every_layer(self):
        self.llama(lambda c: LLAMA_LOG + llama_row(512, 0, 812.5, 3.1) + "\n")
        e = self.run1("llama-prompt-gpu")
        self.assertEqual((e["value"], e["spread"], e["api"], e["reason"]), (812.5, 3.1, "Vulkan", None))
        c = self.h.argv("llama-bench")[0]
        self.assertEqual(c[c.index("-p") + 1], "512")
        self.assertEqual(c[c.index("-n") + 1], "0")
        self.assertEqual(c[c.index("-ngl") + 1], str(bench.LLAMA_ALL_LAYERS))

    def test_llama_with_layers_left_on_the_cpu_fell_back(self):
        self.llama(lambda c: LLAMA_LOG.replace("29/29", "12/29") + llama_row(0, 128, 40.0, 0.2) + "\n")
        self.assertEqual(self.run1("llama-gen-gpu")["reason"], "fell_back")

    def test_llama_generation_on_the_cpu(self):
        self.llama(lambda c: llama_row(0, 128, 25.25, 0.1, ngl=0) + "\n")
        e = self.run1("llama-gen-cpu")
        self.assertEqual((e["value"], e["api"], e["reason"]), (25.25, None, None))
        c = self.h.argv("llama-bench")[0]
        self.assertEqual((c[c.index("-ngl") + 1], c[c.index("-p") + 1], c[c.index("-n") + 1], c[c.index("-t") + 1]),
                         ("0", "0", "128", str(CPUS)))

    def test_mlx_only_with_a_gpu_build_in_the_stack(self):
        self.assertEqual(self.run1("mlx-matmul-fp16")["reason"], "no_build")
        self.h.tools["python3"] = lambda c: '{"status": "no_build"}\n'
        self.assertEqual(self.run1("mlx-matmul-fp16")["reason"], "no_build")
        self.h.tools["python3"] = lambda c: '{"status": "ok", "tflops": 3.21}\n'
        e = self.run1("mlx-matmul-fp16")
        self.assertEqual((e["value"], e["unit"], e["reason"]), (3.21, "TFLOPS", None))
        self.assertEqual(self.h.argv("python3")[-1][-3:], ["4096", "5", "50"])
        self.h.tools["python3"] = lambda c: '{"status": "fell_back", "tflops": 0.1}\n'
        self.assertEqual(self.run1("mlx-matmul-fp16")["reason"], "fell_back")

    def test_mlx_synchronises_after_every_product_as_mlx_bench_does(self):
        stub = pathlib.Path(tempfile.mkdtemp()) / "mlx"
        self.addCleanup(shutil.rmtree, stub.parent)
        stub.mkdir()
        (stub / "__init__.py").write_text("")
        (stub / "core.py").write_text("""import sys
gpu, float16 = "gpu", "float16"
class A:
    def astype(self, t): return self
    def __matmul__(self, o): return "product"
class random:
    normal = staticmethod(lambda shape: A())
def is_available(d): return True
def set_default_device(d): pass
def default_device(): return "Device(gpu, 0)"
def eval(*a): sys.stderr.write("E" if a == ("product",) else "e")
def synchronize(): sys.stderr.write("S")
""")
        r = subprocess.run([sys.executable, "-c", bench.MLX_PROBE, "64", "2", "3"], env={**os.environ, "PYTHONPATH": str(stub.parent)},
                           capture_output=True, text=True, check=True)
        self.assertEqual(r.stderr, "eS" + "ES" * 5)
        self.assertEqual(json.loads(r.stdout)["status"], "ok")


class Power(Case):
    def sensor(self, microwatts="7250000"):
        self.h.write("sys/class/hwmon/hwmon2/name", "macsmc_hwmon")
        self.h.write("sys/class/hwmon/hwmon2/power1_label", "Total System Power")
        self.h.write("sys/class/hwmon/hwmon2/power1_input", microwatts)
        self.h.write("sys/class/hwmon/hwmon2/power2_label", "Heatpipe Power")
        self.h.write("sys/class/hwmon/hwmon2/power2_input", "99000000")

    def test_no_pstr_is_no_comparable_sensor(self):
        self.h.write("sys/class/hwmon/hwmon0/name", "macsmc_hwmon")
        self.h.write("sys/class/hwmon/hwmon0/power1_label", "Heatpipe Power")
        self.h.write("sys/class/hwmon/hwmon0/power1_input", "1000000")
        self.assertEqual(self.run1("power-idle")["reason"], "no_comparable_sensor")
        self.sensor("0")
        self.assertEqual(self.run1("power-idle")["reason"], "no_comparable_sensor")

    def test_a_charging_battery_is_no_comparison(self):
        self.sensor()
        self.h.write("sys/class/power_supply/macsmc-battery/type", "Battery")
        self.h.write("sys/class/power_supply/macsmc-battery/status", "Charging")
        self.assertEqual(self.run1("power-idle")["reason"], "battery_charging")
        self.h.write("sys/class/power_supply/macsmc-battery/status", "Not charging")
        self.assertIsNone(self.run1("power-idle")["reason"])

    def test_idle_is_the_mean_over_60_s_of_pstr(self):
        self.sensor()
        t0 = self.h.t
        e = self.run1("power-idle")
        self.assertEqual((e["value"], e["spread"], e["unit"], e["api"]), (7.25, 0.0, "W", "macsmc-hwmon"))
        self.assertGreaterEqual(self.h.t - t0, 60)

    def test_all_core_load_is_the_energy_of_a_fixed_count_of_iterations(self):
        self.sensor("20000000")
        self.pin("coremark", {"coremark/coremark": ("", 0o755)})
        self.ctx.power_interval = 0.01

        def work(c):
            time.sleep(0.2)
            return COREMARK_SHORT
        self.h.tools["coremark"] = work
        e = self.run1("power-all-core")
        self.assertIsNone(e["reason"])
        self.assertEqual(e["unit"], "J")
        self.assertAlmostEqual(e["value"], 4.0, delta=1.5)
        c = self.h.argv("coremark")[0]
        self.assertEqual((c[1], int(c[5]) * CPUS), (f"M{CPUS}", bench.POWER_COREMARK_ITERATIONS))

    def test_playback_is_the_energy_of_every_frame_in_hardware_at_its_own_pace(self):
        self.sensor("10000000")
        self.ctx.power_interval = 0.01

        def ff(c):
            if "-hwaccels" in c:
                return " v4l2request\n"
            time.sleep(0.1)
            return FFMPEG
        self.h.tools["ffmpeg"], self.h.tools["ffprobe"] = ff, (lambda c: "300\n")
        self.h.write("dev/media0", "")
        self.pin("clip-h264-1080p")
        e = self.run1("power-playback-1080p")
        self.assertIsNone(e["reason"])
        self.assertAlmostEqual(e["value"], 1.0, delta=0.6)
        self.assertIn("-re", [c for c in self.h.argv("ffmpeg") if "-hwaccels" not in c][0])


class OmarchyOnly(Case):
    def test_boot_time(self):
        self.h.tools["systemd-analyze"] = lambda c: BOOT
        e = self.run1("boot-time")
        self.assertEqual((e["value"], e["unit"], e["higher_is_better"], e["category"]), (73.539, "s", False, "Omarchy only"))


class Browser(Case):
    def test_websocket_round_trip(self):
        a, b = socket.socketpair()
        try:
            a.sendall(bench.ws_frame(b'{"id":1}', mask=True))
            self.assertEqual(bench.ws_recv(b), b'{"id":1}')
            big = b"x" * 70000
            t = threading.Thread(target=b.sendall, args=(bench.ws_frame(big, mask=False),))
            t.start()
            self.assertEqual(bench.ws_recv(a), big)
            t.join()
        finally:
            a.close()
            b.close()

    def test_every_web_metric_has_its_page(self):
        for mid in ("motionmark", "speedometer", "jetstream"):
            self.assertIn(mid, bench.METRICS)
            for k in ("url", "done", "result"):
                self.assertIn(k, bench.APPS[mid])
        self.assertIn("startAutomatically", bench.APPS["speedometer"]["url"])

    def test_result_from_page(self):
        self.assertEqual(bench.page_result({"value": 25.3, "spread": 0.4}), (25.3, 0.4))
        with self.assertRaises(bench.ParseError):
            bench.page_result({"value": None})

    def test_chrome_runs_headless_on_the_gpu_at_1920_by_1080_and_dpr_2(self):
        f = bench.BROWSER_FLAGS
        self.assertIn("--headless", f)
        self.assertIn("--enable-gpu", f)
        self.assertIn("--window-size=1920,1080", f)
        self.assertIn("--screen-info={0,0 3840x2160 devicePixelRatio=2}", f)
        self.assertFalse([x for x in f if x.startswith("--ozone-platform")])
        for mid in ("motionmark", "speedometer", "jetstream"):
            s = self.metric(mid)["settings"]
            self.assertEqual((s["width"], s["height"], s["device_pixel_ratio"]), (1920, 1080, bench.DEVICE_PIXEL_RATIO))
        self.assertEqual(self.metric("motionmark")["settings"]["refresh_hz"], bench.REFRESH_HZ)

    def test_devtools_port_file(self):
        with tempfile.TemporaryDirectory() as d:
            pathlib.Path(d, "DevToolsActivePort").write_text("41234\n/devtools/browser/abc\n")
            self.assertEqual(bench.devtools_port(d), 41234)


class Conditions(Case):
    def supply(self, name, typ, online=None):
        self.h.write(f"sys/class/power_supply/{name}/type", typ + "\n")
        if online is not None:
            self.h.write(f"sys/class/power_supply/{name}/online", f"{online}\n")

    def test_on_ac(self):
        self.assertEqual(bench.on_ac(self.h), (True, False))
        self.supply("macsmc-battery", "Battery")
        self.supply("macsmc-ac", "Mains", 0)
        self.assertEqual(bench.on_ac(self.h), (False, True))
        self.supply("macsmc-ac", "Mains", 1)
        self.assertEqual(bench.on_ac(self.h), (True, True))

    def test_refused_on_battery_and_when_busy(self):
        self.supply("macsmc-battery", "Battery")
        self.supply("macsmc-ac", "Mains", 0)
        with self.assertRaises(bench.Refused) as e:
            bench.conditions(self.h)
        self.assertEqual(e.exception.code, "not_on_ac")
        self.supply("macsmc-ac", "Mains", 1)
        with self.assertRaises(bench.Refused) as e:
            bench.conditions(self.h, busy_fn=lambda h: 80.0)
        self.assertEqual(e.exception.code, "not_idle")

    def test_profile_and_backlight_set_then_put_back(self):
        profile = ["power-saver"]

        def ppc(c):
            if c[1] == "set":
                profile[0] = c[2]
                return ""
            return profile[0] + "\n"
        self.h.tools["powerprofilesctl"] = ppc
        self.h.write("sys/class/backlight/apple-panel-bl/max_brightness", "500")
        self.h.write("sys/class/backlight/apple-panel-bl/brightness", "120")
        c, saved = bench.conditions(self.h, busy_fn=lambda h: 1.0)
        self.assertEqual((c["power_profile"], c["backlight_percent"], c["on_ac"], c["cpu_busy_percent"]), ("balanced", 50, True, 1.0))
        self.assertEqual(self.h.read("sys/class/backlight/apple-panel-bl/brightness"), "250")
        bench.restore(self.h, saved)
        self.assertEqual(self.h.read("sys/class/backlight/apple-panel-bl/brightness"), "120")
        self.assertEqual(profile[0], "power-saver")

    def test_cool_down_stops_under_threshold_or_at_the_limit(self):
        self.h.write("sys/class/thermal/thermal_zone0/temp", "80000")
        self.h.temps = [70000, 60000, 49000, 40000]
        self.assertEqual(bench.cool_down(self.h, limit=60, threshold=50, step=5), (15, 49.0))
        self.h.temps = []
        self.h.write("sys/class/thermal/thermal_zone0/temp", "90000")
        self.assertEqual(bench.cool_down(self.h, limit=60, threshold=50, step=5), (60, 90.0))

    def test_cool_down_with_no_sensor_waits_30_s(self):
        self.assertEqual(bench.cool_down(self.h), (30, None))

    def test_busy_from_proc_stat(self):
        stats = iter(["cpu  100 0 100 800 0 0 0 0 0 0\n", "cpu  150 0 150 900 0 0 0 0 0 0\n"])
        orig = self.h.read
        self.h.read = lambda rel: next(stats) if rel == "proc/stat" else orig(rel)
        self.assertAlmostEqual(bench.busy(self.h, 1), 50.0)


class Output(Case):
    def allowed(self):
        """Every string bench.json may hold: the set's own words and the reason codes."""
        words = set(bench.REASONS) | {self.set["set"]}
        for m in self.set["metrics"]:
            words |= {m["id"], m["category"], m["version"], m["unit"], (m.get("omarchy") or {}).get("api")}
        return words - {None}

    def strings(self, x):
        if isinstance(x, str):
            yield x
        elif isinstance(x, dict):
            for k, v in x.items():
                yield from self.strings(v)
        elif isinstance(x, list):
            for v in x:
                yield from self.strings(v)

    def main(self, *extra, impl=None):
        out = pathlib.Path(self.d.name, "out", "bench.json")
        out.parent.mkdir(exist_ok=True)
        setf = pathlib.Path(self.d.name, "set.json")
        setf.write_text(json.dumps(self.set))
        code = bench.main(["--set", str(setf), "--cache", str(self.cache), "--work", str(self.work),
                           "--out", str(out), *extra], host=self.h, stderr=self.log, impl=impl)
        return code, json.loads(out.read_text()), json.loads((out.parent / "bench-conditions.json").read_text())

    def test_the_full_list_on_a_mac_with_nothing_carries_no_free_text(self):
        def noisy(c):
            return bench.Ran(1, "Error: /home/alice/.config is not writable by alice on alice-macbook", 0)
        for t in ("systemd-analyze", "ffmpeg", "ffprobe", "python3", "fincore", "powerprofilesctl", "hyprctl"):
            self.h.tools[t] = noisy
        code, rows, cond = self.main("--list", "full")
        self.assertEqual([r["metric"] for r in rows], [m["id"] for m in SET["metrics"]])
        allowed = self.allowed()
        for r in rows:
            self.assertEqual(tuple(r), bench.FIELDS)
            self.assertIn(r["reason"], bench.REASONS)
            self.assertIsNone(r["value"])
            self.assertEqual(r["set"], "bench-1")
            for s in self.strings(r):
                self.assertIn(s, allowed, r)
        for s in self.strings(cond):
            self.assertIn(s, allowed | {"quick", "full", "balanced"}, cond)
        self.assertNotIn("alice", json.dumps(rows) + json.dumps(cond))
        self.assertIn("alice", self.log.getvalue())
        self.assertEqual(code, 1)
        by = {r["metric"]: r["reason"] for r in rows}
        self.assertEqual(by["fio-seq-read"], "placeholder_file")
        self.assertEqual(by["boot-time"], "error")
        self.assertEqual(by["mlx-matmul-fp16"], "error")
        self.assertEqual(by["power-idle"], "no_comparable_sensor")

    def test_quick_list_is_one_metric_per_category(self):
        code, rows, cond = self.main("--list", "quick", impl={})
        self.assertEqual([r["metric"] for r in rows], [m["id"] for m in SET["metrics"] if m["list"] == "quick"])
        self.assertEqual({r["reason"] for r in rows}, {"not_run"})
        self.assertEqual((cond["set"], cond["list"], cond["refused"]), ("bench-1", "quick", None))
        self.assertEqual(code, 0)

    def test_only_runs_the_metrics_named_in_the_sets_order(self):
        full = [m["id"] for m in SET["metrics"] if m.get("omarchy")]
        pick = [full[5], full[1]]
        code, rows, cond = self.main("--list", "full", "--only", ",".join(pick), impl={})
        self.assertEqual(code, 0)
        self.assertEqual([r["metric"] for r in rows], [full[1], full[5]])
        self.assertEqual(cond["list"], "full")

    def test_only_refuses_an_id_outside_the_list_and_measures_nothing(self):
        quick = [m["id"] for m in SET["metrics"] if m["list"] == "quick"]
        full_only = next(m["id"] for m in SET["metrics"] if m["list"] == "full")
        out = pathlib.Path(self.d.name, "out", "bench.json")
        for only, says in (("nope", "nope"), (quick[0] + ",nope", "nope"), (full_only, full_only),
                           (quick[0] + "," + quick[0], "twice"), ("", "--only")):
            with self.subTest(only=only):
                self.log.seek(0)
                self.log.truncate()
                with self.assertRaises(FileNotFoundError):
                    self.main("--list", "quick", "--only", only, impl={})
                self.assertIn("--only", self.log.getvalue())
                self.assertIn(says, self.log.getvalue())
                self.assertFalse(out.exists())

    def test_only_is_checked_before_the_conditions_are_set(self):
        self.h.write("sys/class/power_supply/bat/type", "Battery")
        self.h.write("sys/class/power_supply/ac/type", "Mains")
        self.h.write("sys/class/power_supply/ac/online", "0")
        setf = pathlib.Path(self.d.name, "set.json")
        setf.write_text(json.dumps(self.set))
        code = bench.main(["--set", str(setf), "--only", "nope", "--cache", str(self.cache), "--work", str(self.work),
                           "--out", str(pathlib.Path(self.d.name, "bench.json"))], host=self.h, stderr=self.log)
        self.assertEqual(code, 2)
        self.assertNotIn("conditions:", self.log.getvalue())

    def test_the_conditions_carry_labds_field_names(self):
        labd = {"on_ac", "cpu_busy_percent", "backlight_percent", "power_profile", "low_power_mode", "refresh_hz", "free_percent"}
        _, _, cond = self.main("--list", "quick", impl={})
        self.assertEqual(set(cond) - labd, {"set", "list", "refused", "battery"})

    def test_values_and_spreads_with_a_cool_down_before_each(self):
        impl = {"boot-time": lambda ctx, m: bench.Value(9.5), "power-idle": lambda ctx, m: bench.Value(6.0, 0.25)}
        self.set["metrics"] = [m for m in self.set["metrics"] if m["id"] in impl]
        code, rows, _ = self.main("--list", "full", impl=impl)
        self.assertEqual(code, 0)
        self.assertEqual([(r["metric"], r["value"], r["spread"], r["reason"]) for r in rows],
                         [("power-idle", 6.0, 0.25, None), ("boot-time", 9.5, None, None)])
        self.assertEqual(rows[0]["cool_down"], 30)
        self.assertEqual(rows[0]["api"], "macsmc-hwmon")

    def test_a_row_that_fell_back_does_not_fail_the_run_and_an_error_does(self):
        def fell(ctx, m):
            raise bench.Reason("fell_back")

        def boom(ctx, m):
            raise RuntimeError("crashed reading /home/alice")
        self.set["metrics"] = [m for m in self.set["metrics"] if m["id"] in ("boot-time", "power-idle")]
        self.assertEqual(self.main("--list", "full", impl={"boot-time": fell, "power-idle": fell})[0], 0)
        code, rows, _ = self.main("--list", "full", impl={"boot-time": boom, "power-idle": fell})
        self.assertEqual(code, 1)
        self.assertNotIn("alice", json.dumps(rows))

    def test_refused_off_ac_measures_nothing(self):
        self.h.write("sys/class/power_supply/bat/type", "Battery")
        self.h.write("sys/class/power_supply/ac/type", "Mains")
        self.h.write("sys/class/power_supply/ac/online", "0")
        code, rows, cond = self.main("--list", "quick")
        self.assertEqual((code, rows, cond["refused"], cond["on_ac"]), (3, [], "not_on_ac", False))

    def test_a_cancelled_run_keeps_what_it_measured(self):
        def cancel(ctx, m):
            raise bench.Cancelled()
        self.set["metrics"] = [m for m in self.set["metrics"] if m["id"] in ("boot-time", "power-idle")]
        code, rows, _ = self.main("--list", "full", impl={"power-idle": lambda ctx, m: bench.Value(6.0), "boot-time": cancel})
        self.assertEqual(code, 143)
        self.assertEqual([r["metric"] for r in rows], ["power-idle"])

    def test_clean_keeps_the_allowlist_and_the_codes(self):
        m = self.metric("7zip")
        e = dict(bench.entry(self.set, m, 1.0), note="free text", reason="it broke")
        c = bench.clean(e)
        self.assertEqual(tuple(c), bench.FIELDS)
        self.assertEqual((c["reason"], c["value"]), ("error", None))
        c = bench.clean(bench.entry(self.set, m, float("nan")))
        self.assertEqual(c["reason"], "error")
        c = bench.clean(bench.entry(self.set, m, 5.0, cool={"seconds": 3, "temp_c": 40.0, "sensor": "x"}))
        self.assertEqual((c["value"], c["cool_down"]), (5.0, 3.0))

    def test_the_reason_codes(self):
        self.assertEqual(bench.REASONS, (
            "no_build", "no_driver", "fell_back", "no_comparable_sensor", "not_run", "skipped", "error",
            "disk_too_full", "not_on_ac", "not_idle", "software_renderer", "placeholder_file", "battery_charging"))
        for code in bench.FAILING:
            self.assertIn(code, bench.REASONS)
        with self.assertRaises(ValueError):
            bench.Reason("it fell over")

    def test_every_metric_of_the_set_has_a_runner(self):
        for m in SET["metrics"]:
            self.assertIn(m["id"], bench.METRICS)
        self.assertFalse(set(bench.METRICS) - {m["id"] for m in SET["metrics"]})

    def test_no_benchmark_that_left_remains(self):
        src = (HERE / "bench.py").read_text()
        for gone in ("glmark2", "vkmark", "vkpeak", "kernel_compile", "rtcwake", "geekbench", "chromium"):
            self.assertIsNone(re.search(gone, src, re.I), gone)


if __name__ == "__main__":
    unittest.main()
