import io
import json
import os
import pathlib
import socket
import sys
import tarfile
import tempfile
import threading
import unittest
from unittest import mock

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import bench  # noqa: E402

COREMARK = """[3]crcfinal      : 0xa14c
Correct operation validated. See readme.txt for run and reporting rules.
CoreMark 1.0 : 177922.751872 / GCC14.2.1 20250207 -O2 / Heap / 4:PThreads
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

GLMARK2 = """=======================================================
    glmark2 2023.01
=======================================================
[build] use-vbo=true: FPS: 4500 FrameTime: 0.222 ms
=======================================================
                                  glmark2 Score: 4096
=======================================================
"""

VKMARK = """[vertex] device-local=true: FPS: 9000 FrameTime: 0.111 ms
=======================================================
                                   vkmark Score: 5127
=======================================================
"""

BOOT = """Startup finished in 4.611s (firmware) + 1.284s (loader) + 2.040s (kernel) + 3.501s (initrd) + 1min 2.103s (userspace) = 1min 13.539s
graphical.target reached after 1min 2.000s in userspace.
"""

GEEKBENCH = """Benchmark Summary
-----------------
  Single-Core Score            2690
    File Compression           2311
  Multi-Core Score            14740
    File Compression          11201
"""

CLPEAK = """Platform: rusticl
  Device: Apple M2 Max (G14C B1)
    Driver version  : 25.0.0 (Linux ARM64)

    Global memory bandwidth (GBPS)
      float   : 300.12

    Single-precision compute (GFLOPS)
      float   : 1234.56
      float2  : 2345.67
      float4  : 3456.78
      float8  : 3400.12

    Half-precision compute (GFLOPS)
      half    : 9999.99
"""

CLPEAK_3 = """Backend: OpenCL
  Device 0: Apple M2 Max (G14C B1)
    Driver version   : 26.2.99

    Single-precision compute
      float    : 9.45 TFLOPS
      float2   : 10.2 TFLOPS
      float4   : 980.5 GFLOPS

"""

VKPEAK = """device       = Apple M2 Max (G14C B1)

fp32-scalar  = 3456.78 GFLOPS
fp32-vec4    = 3567.89 GFLOPS

fp16-scalar  = 7000.00 GFLOPS
"""

FFMPEG = ("frame=  120 fps=0.0 q=-0.0 size=N/A time=00:00:04.00\r"
          "frame=  300 fps=180 q=-0.0 Lsize=N/A time=00:00:10.00 bitrate=N/A speed=6.01x\n"
          "bench: utime=0.501s stime=0.320s rtime=1.500s\n")

DMESG = """[   90.0] PM: suspend entry (s2idle)
[   90.1] PM: suspend of devices complete after 999.000 msecs
[   95.0] PM: suspend exit
[  100.0] PM: suspend entry (s2idle)
[  100.2] PM: suspend of devices complete after 123.500 msecs
[  100.3] PM: late suspend of devices complete after 1.500 msecs
[  115.0] PM: early resume of devices complete after 5.000 msecs
[  115.4] PM: resume of devices complete after 345.000 msecs
[  115.5] PM: suspend exit
"""


class Parsers(unittest.TestCase):
    def test_7z_total_rating(self):
        self.assertEqual(bench.parse_7z(SEVEN_ZIP), 50450)

    def test_scores(self):
        self.assertEqual(bench.parse_score(GLMARK2, "glmark2"), 4096)
        self.assertEqual(bench.parse_score(VKMARK, "vkmark"), 5127)

    def test_boot_total(self):
        self.assertAlmostEqual(bench.parse_boot(BOOT), 73.539)
        self.assertAlmostEqual(bench.parse_boot("Startup finished in 2.5s (kernel) + 820ms (userspace) = 3.320s\n"), 3.32)

    def test_coremark(self):
        self.assertEqual(bench.parse_coremark(COREMARK), 177922.751872)
        with self.assertRaisesRegex(bench.ParseError, "at least 10 secs"):
            bench.parse_coremark("Iterations/Sec   : 900.0\nERROR! Must execute for at least 10 secs for a valid result!\n")

    def test_geekbench(self):
        self.assertEqual(bench.parse_geekbench(GEEKBENCH), {"single": 2690, "multi": 14740})

    def test_clpeak_best_single_precision(self):
        self.assertAlmostEqual(bench.parse_clpeak(CLPEAK), 3456.78)

    def test_clpeak_3_best_single_precision_in_gflops(self):
        self.assertAlmostEqual(bench.parse_clpeak(CLPEAK_3), 10200.0)

    def test_vkpeak_fp32_vec4(self):
        self.assertAlmostEqual(bench.parse_vkpeak(VKPEAK), 3567.89)

    def test_ffmpeg_fps_from_frames_and_real_time(self):
        self.assertAlmostEqual(bench.parse_ffmpeg(FFMPEG), 200.0)

    def test_suspend_last_cycle_only(self):
        self.assertEqual(bench.parse_suspend(DMESG), (125.0, 350.0))

    def test_garbage_is_an_error(self):
        for fn in (bench.parse_7z, bench.parse_boot, bench.parse_clpeak, bench.parse_vkpeak, bench.parse_ffmpeg):
            with self.assertRaises(bench.ParseError):
                fn("nothing here")
        with self.assertRaises(bench.ParseError):
            bench.parse_score("", "glmark2")
        with self.assertRaises(bench.ParseError):
            bench.parse_suspend("")


class FakeHost(bench.Host):
    """A sysfs tree in a temp dir and a clock that moves only when slept."""

    def __init__(self, root):
        super().__init__(root=root)
        self.t = 0.0
        self.temps = []
        self.ran = []

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

    def run(self, cmd, **kw):
        self.ran.append(cmd)
        if cmd[:2] == ["powerprofilesctl", "get"]:
            return bench.Ran(0, "balanced\n", 0.0)
        return bench.Ran(0, "", 0.0)


class Conditions(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.h = FakeHost(self.d.name)

    def tearDown(self):
        self.d.cleanup()

    def supply(self, name, typ, online=None):
        self.h.write(f"sys/class/power_supply/{name}/type", typ + "\n")
        if online is not None:
            self.h.write(f"sys/class/power_supply/{name}/online", f"{online}\n")

    def test_ac_online_is_on_ac(self):
        self.supply("macsmc-battery", "Battery")
        self.supply("macsmc-ac", "Mains", 1)
        self.assertTrue(bench.on_ac(self.h)[0])

    def test_battery_with_ac_offline_is_refused(self):
        self.supply("macsmc-battery", "Battery")
        self.supply("macsmc-ac", "Mains", 0)
        ok, why = bench.on_ac(self.h)
        self.assertFalse(ok)
        self.assertIn("battery", why)

    def test_a_desktop_mac_has_no_battery_and_is_on_ac(self):
        self.assertTrue(bench.on_ac(self.h)[0])

    def test_cool_down_stops_under_threshold(self):
        self.h.write("sys/class/thermal/thermal_zone0/temp", "80000")
        self.h.temps = [70000, 60000, 49000, 40000]
        waited, temp = bench.cool_down(self.h, limit=60, threshold=50, step=5)
        self.assertEqual(waited, 15)
        self.assertEqual(temp, 49.0)

    def test_cool_down_gives_up_at_the_limit(self):
        self.h.write("sys/class/thermal/thermal_zone0/temp", "90000")
        waited, temp = bench.cool_down(self.h, limit=60, threshold=50, step=5)
        self.assertEqual(waited, 60)
        self.assertEqual(temp, 90.0)

    def test_hwmon_cpu_sensor_wins_over_nothing(self):
        self.h.write("sys/class/hwmon/hwmon3/temp1_input", "45000")
        self.assertEqual(bench.cpu_temp(self.h), 45.0)

    def test_busy_from_proc_stat(self):
        stats = iter([
            "cpu  100 0 100 800 0 0 0 0 0 0\n",
            "cpu  150 0 150 900 0 0 0 0 0 0\n",
        ])
        orig = self.h.read
        self.h.read = lambda rel: next(stats) if rel == "proc/stat" else orig(rel)
        self.assertAlmostEqual(bench.busy(self.h, 1), 50.0)

    def test_conditions_refuse_on_battery(self):
        self.supply("macsmc-battery", "Battery")
        self.supply("macsmc-ac", "Mains", 0)
        with self.assertRaises(bench.Refused):
            bench.conditions(self.h)

    def test_conditions_refuse_when_busy(self):
        self.h.busy_override = 80.0
        with self.assertRaises(bench.Refused) as e:
            bench.conditions(self.h, busy_fn=lambda h: 80.0)
        self.assertIn("busy", str(e.exception))

    def test_conditions_fix_profile_and_brightness(self):
        self.h.write("sys/class/backlight/apple-panel-bl/max_brightness", "500")
        self.h.write("sys/class/backlight/apple-panel-bl/brightness", "120")
        c = bench.conditions(self.h, busy_fn=lambda h: 1.0)
        self.assertEqual(c["power_profile"], "balanced")
        self.assertIn(["powerprofilesctl", "set", "balanced"], self.h.ran)
        self.assertEqual(self.h.read("sys/class/backlight/apple-panel-bl/brightness"), "250")
        self.assertEqual(c["brightness"], {"apple-panel-bl": {"was": 120, "set": 250, "max": 500}})
        bench.restore(self.h, c)
        self.assertEqual(self.h.read("sys/class/backlight/apple-panel-bl/brightness"), "120")


class Runner(unittest.TestCase):
    KEYS = {"name", "version", "value", "unit", "higher_is_better", "spread", "seconds"}

    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.h = FakeHost(self.d.name)

    def tearDown(self):
        self.d.cleanup()

    def test_tiers(self):
        self.assertEqual(bench.TIERS["default"], ["speedometer-chrome", "motionmark-chrome", "7zip", "glmark2", "vkmark", "boot"])
        ext = bench.TIERS["extended"]
        self.assertEqual(ext[:6], bench.TIERS["default"])
        for b in ("speedometer-chromium", "jetstream-chrome", "geekbench", "coremark", "kernel-compile", "vkpeak", "clpeak", "ffmpeg-vaapi", "suspend"):
            self.assertIn(b, ext)
        for name in ext:
            self.assertIn(name, bench.BENCHES)

    def test_every_needed_item_is_pinned(self):
        m = bench.load_manifest(HERE / "testdata" / "manifest.json")
        for name, b in bench.BENCHES.items():
            for item in b.needs:
                self.assertIn(item, m, f"{name} needs {item}")
                self.assertRegex(m[item]["sha256"], r"^[0-9a-f]{64}$")
                self.assertTrue(m[item]["url"].startswith(("https://", "http://")))
                self.assertTrue(m[item]["version"])

    def test_json_shape_with_fakes(self):
        benches = {
            "a": bench.Bench(lambda ctx: [bench.Measure("a", "1.0", 10.0, "score", True, 0.5)]),
            "b": bench.Bench(lambda ctx: (_ for _ in ()).throw(bench.Skip("no vkpeak on this Mac"))),
            "c": bench.Bench(lambda ctx: (_ for _ in ()).throw(RuntimeError("crashed"))),
            "d": bench.Bench(lambda ctx: [bench.Measure("d-single", "6", 1, "score", True, None),
                                          bench.Measure("d-multi", "6", 2, "score", True, None)]),
        }
        out = bench.run_tier(["a", "b", "c", "d"], benches, self.h, ctx=None, cool=lambda h: (0, None))
        json.dumps(out)
        self.assertEqual([r["name"] for r in out], ["a", "b", "c", "d-single", "d-multi"])
        for r in out:
            self.assertTrue(self.KEYS <= set(r), r)
        self.assertEqual(out[0]["value"], 10.0)
        self.assertEqual(out[0]["spread"], 0.5)
        self.assertIsNone(out[1]["value"])
        self.assertEqual(out[1]["skipped"], "no vkpeak on this Mac")
        self.assertIn("crashed", out[2]["error"])
        self.assertEqual(bench.exit_code(out), 1)
        self.assertEqual(bench.exit_code(out[:2]), 0)

    def test_cool_down_is_recorded_per_benchmark(self):
        benches = {"a": bench.Bench(lambda ctx: [bench.Measure("a", "1", 1, "s", False, None)])}
        out = bench.run_tier(["a"], benches, self.h, ctx=None, cool=lambda h: (12, 48.5))
        self.assertEqual(out[0]["cool_down"], {"seconds": 12, "temp_c": 48.5})


def pacman_pkg(cache, sha, files):
    """A pacman package in the cache under sha: {path: (text, mode) or ('->', target)}."""
    with tempfile.TemporaryDirectory() as d:
        for rel, (a, b) in files.items():
            p = pathlib.Path(d, rel)
            p.parent.mkdir(parents=True, exist_ok=True)
            if a == "->":
                os.symlink(b, p)
            else:
                p.write_text(a)
                os.chmod(p, b)
        with tarfile.open(pathlib.Path(cache, sha), "w:xz") as t:
            for rel in files:
                t.add(pathlib.Path(d, rel), arcname=rel, recursive=False)


class Packages(unittest.TestCase):
    """Programs run from their pinned package unpacked in a temp dir, never installed."""

    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.cache = pathlib.Path(self.d.name, "cache")
        self.work = pathlib.Path(self.d.name, "work")
        self.cache.mkdir()
        self.work.mkdir()
        self.m = bench.load_manifest(HERE / "testdata" / "manifest.json")
        self.ctx = bench.Ctx(bench.Host(), self.m, self.cache, self.work)

    def tearDown(self):
        self.d.cleanup()

    def test_7zip_runs_the_program_not_the_wrapper_that_names_usr_lib(self):
        out = "\n".join("echo '" + l + "'" for l in SEVEN_ZIP.strip().split("\n"))
        pacman_pkg(self.cache, self.m["7zip"]["sha256"], {
            "usr/bin/7z": ('#!/bin/sh\nexec /usr/lib/7zip/7z "$@"\n', 0o755),
            "usr/lib/7zip/7z": ("#!/bin/sh\n" + out + "\n", 0o755),
            "usr/lib/7zip/7z.so": ("", 0o644),
        })
        ms = bench.seven_zip(self.ctx)
        self.assertEqual(ms[0].value, 50450)

    def test_vkmark_finds_the_pinned_assimp(self):
        self.assertIn("assimp", bench.BENCHES["vkmark"].needs)
        self.assertIn("assimp", bench.needs("default"))
        score = "\n".join("echo '" + l + "'" for l in VKMARK.strip().split("\n"))
        pacman_pkg(self.cache, self.m["vkmark"]["sha256"], {
            "usr/bin/vkmark": ("#!/bin/sh\nfor d in $(echo \"$LD_LIBRARY_PATH\" | tr : ' '); do\n"
                               "  [ -e \"$d/libassimp.so.6\" ] && { " + score.replace("\n", "; ") + "; exit 0; }\ndone\n"
                               "echo 'error while loading shared libraries: libassimp.so.6'; exit 127\n", 0o755),
            "usr/lib/vkmark/wayland.so": ("", 0o644),
            "usr/share/vkmark/models/cube.ply": ("", 0o644),
        })
        pacman_pkg(self.cache, self.m["assimp"]["sha256"], {
            "usr/lib/libassimp.so.6.0.6": ("", 0o755),
            "usr/lib/libassimp.so.6": ("->", "libassimp.so.6.0.6"),
        })
        ms = bench.vkmark(self.ctx)
        self.assertEqual(ms[0].value, 5127)

    def clpeak_pkg(self, out):
        lines = "\n".join("echo '" + l + "'" for l in out.strip().split("\n"))
        pacman_pkg(self.cache, self.m["clpeak"]["sha256"], {
            "usr/bin/clpeak": ('#!/bin/sh\n[ "$*" = "--opencl --single-precision-compute" ] || '
                               '{ echo "clpeak: unknown option $1"; exit 1; }\n' + lines + "\n", 0o755),
        })

    def test_clpeak_3_runs_opencl_single_precision(self):
        self.clpeak_pkg(CLPEAK_3)
        self.assertAlmostEqual(bench.clpeak(self.ctx)[0].value, 10200.0)

    def test_clpeak_with_no_opencl_platform_is_skipped(self):
        self.clpeak_pkg("Backend: OpenCL\nclGetPlatformIDs (-1001)\nno platforms found")
        with self.assertRaises(bench.Skip):
            bench.clpeak(self.ctx)


class Builds(unittest.TestCase):
    """Benchmarks built on the Mac from pinned source, with gcc."""

    class Host(bench.Host):
        def __init__(self):
            super().__init__()
            self.ran = []

        def which(self, name):
            return "/usr/bin/" + name

        def run(self, cmd, timeout=None, env=None, gui=False, cwd=None):
            self.ran.append((cmd, env, cwd))
            if cmd[0].endswith(("coremark-1", "coremark-%d" % os.cpu_count())):
                return bench.Ran(0, COREMARK, 20.0)
            return bench.Ran(0, "", 30.0)

    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.cache = pathlib.Path(self.d.name, "cache")
        self.work = pathlib.Path(self.d.name, "work")
        self.cache.mkdir()
        self.work.mkdir()
        self.m = bench.load_manifest(HERE / "testdata" / "manifest.json")
        self.h = self.Host()
        self.ctx = bench.Ctx(self.h, self.m, self.cache, self.work)

    def tearDown(self):
        self.d.cleanup()

    def source(self, item, files):
        pacman_pkg(self.cache, self.m[item]["sha256"], {self.m[item]["root"] + "/" + f: ("", 0o644) for f in files})

    def test_coremark_builds_once_single_and_once_a_thread_per_core(self):
        self.source("coremark", ["core_main.c", "linux64/core_portme.c"])
        ms = bench.coremark(self.ctx)
        self.assertEqual([m.name for m in ms], ["coremark-single", "coremark-multi"])
        self.assertTrue(all(m.higher_is_better and m.unit == "iterations/s" for m in ms))
        builds = [c for c, _, _ in self.h.ran if c[0] == "gcc"]
        self.assertEqual(len(builds), 2)
        self.assertNotIn("-DUSE_PTHREAD", builds[0])
        self.assertIn(f"-DMULTITHREAD={os.cpu_count()}", builds[1])
        self.assertIn("-DPERFORMANCE_RUN=1", builds[1])
        self.assertIn(f"{os.cpu_count()} threads", ms[1].version)

    def test_kernel_compile_finds_the_pinned_bc(self):
        self.assertIn("bc", bench.BENCHES["kernel-compile"].needs)
        self.source("kernel", ["Makefile"])
        pacman_pkg(self.cache, self.m["bc"]["sha256"], {"usr/bin/bc": ("#!/bin/sh\n", 0o755)})
        bench.kernel_compile(self.ctx)
        makes = [(c, e) for c, e, _ in self.h.ran if c[0] == "make"]
        self.assertEqual(len(makes), 2)
        for _, env in makes:
            self.assertTrue(env["PATH"].startswith(str(self.work / "bc" / "usr/bin") + ":"), env)


class Suspend(unittest.TestCase):
    class Host(FakeHost):
        def which(self, name):
            return "/usr/bin/" + name

    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.h = self.Host(self.d.name)
        self.ctx = bench.Ctx(self.h, {}, self.d.name, self.d.name)

    def tearDown(self):
        self.d.cleanup()

    def test_no_rtc_that_can_wake_the_mac_is_a_skip_not_an_error(self):
        # Apple Silicon's macsmc RTC: no wakealarm, no power/wakeup.
        self.h.write("sys/class/rtc/rtc0/name", "macsmc-rtc")
        with mock.patch.object(bench.os, "geteuid", return_value=0):
            with self.assertRaisesRegex(bench.Skip, "wake source"):
                bench.suspend(self.ctx)
            out = bench.run_tier(["suspend"], bench.BENCHES, self.h, self.ctx)
        self.assertIn("wake source", out[0]["skipped"])
        self.assertEqual(bench.exit_code(out), 0)
        self.assertFalse([c for c in self.h.ran if c[0] == "rtcwake"])

    def test_an_rtc_with_an_alarm_wakes_it(self):
        self.h.write("sys/class/rtc/rtc0/name", "macsmc-rtc")
        self.h.write("sys/class/rtc/rtc1/wakealarm", "")
        self.h.write("sys/class/rtc/rtc1/device/power/wakeup", "disabled")
        self.assertEqual(bench.wake_rtc(self.h), "rtc1")
        self.assertEqual(self.h.read("sys/class/rtc/rtc1/device/power/wakeup"), "enabled")


class Browser(unittest.TestCase):
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

    def test_apps_read_their_own_results(self):
        for app in ("speedometer", "motionmark", "jetstream"):
            js = bench.APPS[app]
            self.assertIn("url", js)
            self.assertIn("done", js)
            self.assertIn("result", js)
        self.assertIn("startAutomatically", bench.APPS["speedometer"]["url"])

    def test_result_from_page(self):
        self.assertEqual(bench.page_result({"value": 25.3, "spread": 0.4}), (25.3, 0.4))
        with self.assertRaises(bench.ParseError):
            bench.page_result({"value": None})

    def test_browsers_run_headless_on_the_gpu_so_a_locked_session_still_draws(self):
        # A Lab boot locks the session at once and idle locks it later; a
        # headed window then gets no frames and the pages never finish.
        f = bench.BROWSER_FLAGS
        self.assertIn("--headless", f)
        self.assertIn("--enable-gpu", f)
        self.assertFalse([x for x in f if x.startswith("--ozone-platform")])
        self.assertIn("--screen-info={0,0 3840x2160 devicePixelRatio=2}", f)

    def test_software_rendering_is_refused(self):
        self.assertIsNone(bench.software_gl("ANGLE (Mesa, AGX G13/G14, OpenGL ES 3.2)"))
        for r in ("ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero)), SwiftShader driver)",
                  "ANGLE (Mesa, llvmpipe (LLVM 19.1.7, 128 bits), OpenGL ES 3.2)", None):
            self.assertTrue(bench.software_gl(r))

    def test_devtools_port_file(self):
        with tempfile.TemporaryDirectory() as d:
            pathlib.Path(d, "DevToolsActivePort").write_text("41234\n/devtools/browser/abc\n")
            self.assertEqual(bench.devtools_port(d), 41234)


class Output(unittest.TestCase):
    def test_main_writes_one_json_and_refuses_with_exit_3(self):
        with tempfile.TemporaryDirectory() as d:
            h = FakeHost(d)
            h.write("sys/class/power_supply/bat/type", "Battery")
            h.write("sys/class/power_supply/ac/type", "Mains")
            h.write("sys/class/power_supply/ac/online", "0")
            out = pathlib.Path(d, "bench.json")
            code = bench.main(["--tier", "default", "--manifest", str(HERE / "testdata" / "manifest.json"),
                               "--cache", d, "--work", d, "--out", str(out)], host=h, stderr=io.StringIO())
            self.assertEqual(code, 3)
            self.assertEqual(json.loads(out.read_text()), [])
            cond = json.loads(pathlib.Path(d, "bench-conditions.json").read_text())
            self.assertIn("battery", cond["refused"])


if __name__ == "__main__":
    unittest.main()
