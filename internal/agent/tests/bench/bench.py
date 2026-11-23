#!/usr/bin/env python3
"""The lab's benchmark runner: one Benchmark set's Quick or Full list on a Mac
running Omarchy, by metric id.

lab-agent runs it as root with the list's pinned Omarchy files already
fetched from labd by sha256 into --cache; nothing here reaches the internet.
Programs that draw run as the GUI user inside the Wayland session (runuser
plus that session's environment).

It writes bench.json, one entry per metric holding FIELDS and nothing else,
with a reason from REASONS where there is no value, and bench-conditions.json
beside it. What the tools print goes to stderr, the job's private log, and
never into either file: a tool's text can carry a user name or a path.
"""

import argparse
import base64
import dataclasses
import fcntl
import functools
import glob
import http.server
import json
import math
import os
import pathlib
import re
import shutil
import signal
import socket
import stat
import statistics
import subprocess
import sys
import tarfile
import threading
import time
import traceback
import urllib.request
import zipfile

# The fixed reason codes, the only words bench.json carries besides the
# set's own. labd keeps the same list.
REASONS = (
    "no_build", "no_driver", "fell_back", "no_comparable_sensor", "not_run", "skipped", "error",
    "disk_too_full", "not_on_ac", "not_idle", "software_renderer", "placeholder_file", "battery_charging",
)
# A row with one of these fails the run; any other reason does not.
FAILING = ("error", "software_renderer")

FIELDS = ("set", "metric", "category", "version", "value", "unit", "higher_is_better", "spread",
          "seconds", "cool_down", "api", "capacity", "fill", "reason")

LISTS = ("quick", "full")
SYSTEM = "omarchy"

POWER_PROFILE = "balanced"
BRIGHTNESS = 0.5
COOL_LIMIT = 60
COOL_THRESHOLD = 50.0
BUSY_LIMIT = 25.0

REFRESH_HZ = 60
REFRESH_SLACK = 0.1
DEVICE_PIXEL_RATIO = 2

STORAGE_FREE = 0.20
CACHED_LIMIT = 0.10

# STREAM's arrays are at least four times the largest cache. sysfs does not
# show Apple's system level cache, the largest, so it is taken at the biggest
# any Apple Silicon Mac has.
STREAM_LARGEST_CACHE = 96 << 20
STREAM_CACHE_FACTOR = 4

IDLE_SECONDS = 60
# The all-core load's fixed work: this many CoreMark iterations in all,
# shared among a thread per core. The macOS runner does the same.
POWER_COREMARK_ITERATIONS = 4_000_000
POWER_MAX_W = 1000.0

LLAMA_ALL_LAYERS = 999
ANE_TOOL = "ane-bench"


class Reason(Exception):
    """The metric has no value, for this reason; detail goes to the log only."""

    def __init__(self, code, detail="", capacity=None):
        if code not in REASONS:
            raise ValueError(f"not a reason code: {code}")
        super().__init__(f"{code}: {detail}" if detail else code)
        self.code = code
        self.capacity = capacity


class Refused(Reason):
    """The whole run is refused before anything is measured."""


class ParseError(Exception):
    pass


class Cancelled(BaseException):
    """SIGTERM: lab-agent cancelled the run. Not an Exception, so that no
    benchmark's own handling swallows it and every finally runs."""


# ---------------------------------------------------------------- parsers

def _duration(s):
    total = 0.0
    for n, unit in re.findall(r"(\d+(?:\.\d+)?)(min|ms|us|s|h)\b", s):
        total += float(n) * {"h": 3600, "min": 60, "s": 1, "ms": 1e-3, "us": 1e-6}[unit]
    return total


def parse_boot(text):
    """Total boot time, firmware to userspace, from systemd-analyze."""
    m = re.search(r"Startup finished in .*= (.+?)\s*$", text, re.M)
    if not m:
        raise ParseError("no 'Startup finished' line from systemd-analyze")
    return round(_duration(m.group(1)), 6)


def parse_7z(text):
    """7-Zip's total rating in MIPS: the last number of its Tot: line."""
    m = re.findall(r"^Tot:\s.*?(\d+)\s*$", text, re.M)
    if not m:
        raise ParseError("no Tot: line from 7z b")
    return int(m[-1])


def coremark_valid(text):
    if "Correct operation validated" not in text or re.search(r"^ERROR!.*crc", text, re.M | re.I):
        raise ParseError("CoreMark did not validate its own work")


def parse_coremark(text):
    coremark_valid(text)
    m = re.search(r"^CoreMark 1\.0 : ([0-9.]+)", text, re.M)
    if not m:
        why = re.findall(r"^ERROR!.*", text, re.M)
        raise ParseError("no valid CoreMark result" + (": " + why[0] if why else ""))
    return float(m.group(1))


CLPEAK_SCALE = {"": 1.0, "M": 1e-3, "G": 1.0, "T": 1e3}


def _clpeak_section(text, head):
    sect = re.search(rf"{head}[^\n]*\n((?:[ \t]+\w+[ \t]*:[ \t]*[\d.]+[^\n]*\n?)+)", text)
    if not sect:
        raise ParseError(f"no {head} section from clpeak")
    return sect.group(1)


def parse_clpeak_fp32(text):
    """The best single-precision GFLOPS. clpeak 1.x heads the section
    "(GFLOPS)"; 3.x gives each value its own unit."""
    sect = _clpeak_section(text, "Single-precision compute")
    return max(round(float(v) * CLPEAK_SCALE[u], 3)
               for v, u in re.findall(r":\s*([\d.]+)\s*(?:([MGT])FLOPS)?", sect))


def parse_clpeak_bandwidth(text):
    """The best global memory bandwidth in GB/s, "(GBPS)" in the heading or
    a unit on each value."""
    sect = _clpeak_section(text, "Global memory bandwidth")
    return max(round(float(v) * CLPEAK_SCALE[u], 3)
               for v, u in re.findall(r":\s*([\d.]+)\s*(?:([MGT])B(?:PS|/s))?", sect))


def clpeak_device(text):
    m = re.search(r"Device(?: \d+)?\s*:\s*(.+)", text)
    return m.group(1).strip() if m else None


SOFTWARE = re.compile(r"llvmpipe|softpipe|swrast|swiftshader|lavapipe|software|\bcpu\b", re.I)


def clpeak_verdict(code, text):
    """None when clpeak ran on the physical GPU, else the reason code."""
    if re.search(r"no platforms|CL_PLATFORM_NOT_FOUND|clGetPlatformIDs \(-1001\)", text, re.I):
        return "no_driver"
    if code != 0:
        return "error"
    dev = clpeak_device(text)
    if not dev or SOFTWARE.search(dev):
        return "fell_back"
    return None


def parse_stream(text, kernel):
    """STREAM's best rate of one kernel (Copy, Scale, Add, Triad), in GB/s."""
    if not re.search(r"Solution Validates", text):
        raise ParseError("STREAM did not validate its arrays")
    m = re.search(rf"^{kernel}:\s+([\d.]+)\s+[\d.]+\s+[\d.]+\s+[\d.]+", text, re.M | re.I)
    if not m:
        raise ParseError(f"no {kernel} line from STREAM")
    return round(float(m.group(1)) / 1000, 3)


def parse_fio(text, rw):
    """fio's JSON for one group: (MB/s, its spread), (kIOPS, its spread)."""
    i = text.find("{")
    if i < 0:
        raise ParseError("no JSON from fio")
    try:
        jobs = json.loads(text[i:])["jobs"]
    except (ValueError, KeyError) as e:
        raise ParseError(f"fio's JSON: {e}")
    side = jobs[0]["write" if "write" in rw else "read"]
    if not side.get("io_bytes"):
        raise ParseError("fio moved no bytes")
    return {
        "mbs": round(side["bw_bytes"] / 1e6, 3),
        "mbs_spread": round(side.get("bw_dev", 0) * 1024 / 1e6, 3),
        "kiops": round(side["iops"] / 1e3, 3),
        "kiops_spread": round(side.get("iops_stddev", 0) / 1e3, 3),
    }


def parse_ffmpeg(text):
    """(frames decoded, frames per second of wall time), from -benchmark."""
    frames = re.findall(r"frame=\s*(\d+)", text)
    rtime = re.search(r"bench:.*rtime=([\d.]+)s", text)
    if not frames or not rtime or float(rtime.group(1)) <= 0:
        raise ParseError("no frame count or rtime from ffmpeg")
    n = int(frames[-1])
    return n, round(n / float(rtime.group(1)), 3)


DECODE_NO_DEVICE = re.compile(r"Device creation failed|No device available for decoder|"
                              r"Failed to (?:open|find|probe) .*(?:media|video) device", re.I)
DECODE_FALLBACK = re.compile(r"Failed setup for format|hwaccel initiali[sz]ation returned error|"
                             r"Failed to get (?:a )?(?:HW|hardware) (?:surface )?format|"
                             r"falling back to software|software decoding", re.I)


def decode_verdict(code, text):
    """None when FFmpeg decoded every frame in hardware, else the reason."""
    if DECODE_NO_DEVICE.search(text):
        return "no_driver"
    if code != 0:
        return "error"
    if DECODE_FALLBACK.search(text):
        return "fell_back"
    return None


def parse_llama(text):
    """llama-bench's JSONL rows."""
    rows = []
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("{") and "avg_ts" in line:
            try:
                rows.append(json.loads(line))
            except ValueError:
                pass
    if not rows:
        raise ParseError("no result from llama-bench")
    return rows


def llama_row(rows, prompt, gen):
    for r in rows:
        if int(r.get("n_prompt", -1)) == prompt and int(r.get("n_gen", -1)) == gen:
            return r
    raise ParseError(f"llama-bench gave no row for {prompt} prompt and {gen} generated tokens")


def llama_gpu_verdict(text, rows):
    """None when llama.cpp put every layer on a physical GPU through Vulkan."""
    dev = re.search(r"ggml_vulkan: 0 = (.+)", text)
    if not dev or SOFTWARE.search(dev.group(1)):
        return "fell_back"
    if not any("vulkan" in str(r.get("backends", "")).lower() for r in rows):
        return "fell_back"
    off = re.findall(r"offloaded (\d+)/(\d+) layers to GPU", text)
    if not off or any(a != b for a, b in off):
        return "fell_back"
    return None


def parse_ane(text):
    """ane-bench's (device, inferences/s)."""
    dev = re.search(r"^device:\s*(\S+)", text, re.M)
    v = re.search(r"^inferences/s:\s*([\d.]+)", text, re.M)
    if not dev or not v:
        raise ParseError("no device or inferences/s from ane-bench")
    return dev.group(1).lower(), float(v.group(1))


def parse_mlx(text):
    for line in reversed(text.strip().splitlines()):
        if line.startswith("{"):
            try:
                return json.loads(line)
            except ValueError:
                break
    raise ParseError("no result from the MLX probe")


def page_result(obj):
    if not isinstance(obj, dict) or not isinstance(obj.get("value"), (int, float)):
        raise ParseError("the page gave no score")
    sp = obj.get("spread")
    return obj["value"], sp if isinstance(sp, (int, float)) else None


def devtools_port(profile):
    p = pathlib.Path(profile, "DevToolsActivePort")
    return int(p.read_text().split("\n", 1)[0])


def size_bytes(s):
    m = re.fullmatch(r"(\d+)\s*([KMGT]?)i?B?", str(s).strip(), re.I)
    if not m:
        raise ValueError(f"size {s!r}")
    return int(m.group(1)) << {"": 0, "K": 10, "M": 20, "G": 30, "T": 40}[m.group(2).upper()]


# ---------------------------------------------------------------- the Mac

@dataclasses.dataclass
class Ran:
    code: int
    out: str
    seconds: float


class Host:
    def __init__(self, root="/", gui_user=None, gui_env=()):
        self.root = root
        self.gui_user = gui_user
        self.gui_env = list(gui_env)

    def path(self, rel):
        return os.path.join(self.root, rel)

    def read(self, rel):
        try:
            with open(self.path(rel)) as f:
                return f.read().strip()
        except OSError:
            return None

    def write(self, rel, text):
        with open(self.path(rel), "w") as f:
            f.write(text)

    def glob(self, pattern):
        return sorted(os.path.relpath(p, self.root) for p in glob.glob(self.path(pattern)))

    def now(self):
        return time.monotonic()

    def sleep(self, s):
        time.sleep(s)

    def which(self, name):
        return shutil.which(name)

    def cpus(self):
        try:
            return len(os.sched_getaffinity(0))
        except AttributeError:
            return os.cpu_count() or 1

    def statvfs(self, path):
        return os.statvfs(path)

    def _wrap(self, cmd, gui, env):
        extra = dict(env or {})
        full = dict(os.environ, **extra)
        if gui and self.gui_user and os.geteuid() == 0:
            pre = ["runuser", "-u", self.gui_user, "--", "env", *self.gui_env]
            return pre + [f"{k}={v}" for k, v in extra.items()] + list(cmd), full
        if gui:
            full.update(kv.split("=", 1) for kv in self.gui_env if "=" in kv)
        return list(cmd), full

    def run(self, cmd, timeout=None, env=None, gui=False, cwd=None):
        argv, env = self._wrap([str(c) for c in cmd], gui, env)
        start = time.monotonic()
        try:
            p = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                               text=True, errors="replace", timeout=timeout, env=env, cwd=cwd)
        except FileNotFoundError as e:
            return Ran(127, str(e), 0.0)
        except subprocess.TimeoutExpired as e:
            out = e.stdout if isinstance(e.stdout, str) else (e.stdout or b"").decode(errors="replace")
            return Ran(-1, out + f"\n(timed out after {timeout} s)", time.monotonic() - start)
        return Ran(p.returncode, p.stdout, time.monotonic() - start)

    def spawn(self, cmd, env=None, gui=False):
        argv, env = self._wrap([str(c) for c in cmd], gui, env)
        return subprocess.Popen(argv, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, start_new_session=True)

    def gui_dir(self, parent):
        d = pathlib.Path(parent, f"profile-{os.getpid()}-{int(time.time() * 1000)}")
        d.mkdir(mode=0o700, parents=True)
        if self.gui_user and os.geteuid() == 0:
            import pwd
            pw = pwd.getpwnam(self.gui_user)
            os.chown(d, pw.pw_uid, pw.pw_gid)
        return d

    def lock(self, rel="run/lab-bench.lock"):
        f = open(self.path(rel), "a")
        try:
            fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            f.close()
            raise Refused("not_idle", "another benchmark run holds the lock")
        return f


# ---------------------------------------------------------------- conditions

def supplies(host):
    batteries, mains = [], []
    for t in host.glob("sys/class/power_supply/*/type"):
        kind = host.read(t)
        d = os.path.dirname(t)
        if kind == "Battery":
            batteries.append(d)
        elif kind in ("Mains", "USB", "USB_PD", "USB_C"):
            mains.append(host.read(d + "/online") == "1")
    return batteries, mains


def on_ac(host):
    """(on AC, has a battery). A Mac with no battery counts as on AC."""
    batteries, mains = supplies(host)
    return (not batteries or any(mains)), bool(batteries)


def battery_charging(host):
    return any(host.read(d + "/status") == "Charging" for d in supplies(host)[0])


def cpu_temp(host):
    temps = []
    for rel in host.glob("sys/class/thermal/thermal_zone*/temp") + host.glob("sys/class/hwmon/hwmon*/temp*_input"):
        try:
            temps.append(int(host.read(rel)) / 1000)
        except (TypeError, ValueError):
            pass
    return max(temps) if temps else None


def cool_down(host, limit=COOL_LIMIT, threshold=COOL_THRESHOLD, step=5):
    """Waits until the hottest sensor is under threshold, or limit seconds.
    With no sensor it waits half the limit."""
    temp = cpu_temp(host)
    if temp is None:
        host.sleep(limit // 2)
        return limit // 2, None
    waited = 0
    while temp >= threshold and waited < limit:
        host.sleep(step)
        waited += step
        temp = cpu_temp(host)
    return waited, temp


def busy(host, interval=3):
    def sample():
        line = (host.read("proc/stat") or "").split("\n", 1)[0].split()
        if not line or line[0] != "cpu":
            return None
        v = [int(x) for x in line[1:]]
        return sum(v), v[3] + (v[4] if len(v) > 4 else 0)
    a = sample()
    host.sleep(interval)
    b = sample()
    if not a or not b or b[0] == a[0]:
        return 0.0
    return 100.0 * (1 - (b[1] - a[1]) / (b[0] - a[0]))


def conditions(host, profile=POWER_PROFILE, level=BRIGHTNESS, busy_fn=None, busy_limit=BUSY_LIMIT, c=None):
    """The run's conditions, set: (what bench-conditions.json says, filled
    into c as they are checked, and what restore puts back). Refused when
    not on AC or not idle."""
    ac, battery = on_ac(host)
    c = {} if c is None else c
    c.update({"on_ac": ac, "battery": battery, "cpu_busy_percent": None, "power_profile": None, "backlight_percent": None})
    if not ac:
        raise Refused("not_on_ac")
    load = (busy_fn or busy)(host)
    c["cpu_busy_percent"] = round(load, 1)
    if load > busy_limit:
        raise Refused("not_idle", f"{load:.0f}% CPU")
    saved = {"power_profile": None, "brightness": {}}
    was = host.run(["powerprofilesctl", "get"])
    if was.code == 0 and was.out.strip():
        saved["power_profile"] = was.out.strip()
    host.run(["powerprofilesctl", "set", profile])
    got = host.run(["powerprofilesctl", "get"])
    c["power_profile"] = profile if got.code == 0 and got.out.strip() == profile else None
    for m in host.glob("sys/class/backlight/*/max_brightness"):
        d = os.path.dirname(m)
        try:
            mx, cur = int(host.read(m)), int(host.read(d + "/brightness"))
            host.write(d + "/brightness", str(round(mx * level)))
        except (TypeError, ValueError, OSError):
            continue
        saved["brightness"][d] = cur
        c["backlight_percent"] = round(level * 100)
    host.run(["hyprctl", "dispatch", "dpms", "on"], timeout=10, gui=True)
    return c, saved


def restore(host, saved):
    if not saved:
        return
    for d, was in saved.get("brightness", {}).items():
        try:
            host.write(d + "/brightness", str(was))
        except OSError:
            pass
    if saved.get("power_profile") and saved["power_profile"] != POWER_PROFILE:
        host.run(["powerprofilesctl", "set", saved["power_profile"]])


# ---------------------------------------------------------------- files

class Ctx:
    def __init__(self, host, bset, cache, work, log=sys.stderr):
        self.host, self.set = host, bset
        self.files = {f["id"]: f for f in bset["files"].get(SYSTEM, [])}
        self.cache, self.work, self.logf = pathlib.Path(cache), pathlib.Path(work), log
        self.power_interval = 1.0

    def log(self, what, ran=None):
        if ran is None:
            print(f"-- {what}", file=self.logf, flush=True)
        else:
            print(f"-- {what}: exit {ran.code} after {ran.seconds:.1f} s\n{ran.out.rstrip()}", file=self.logf, flush=True)

    def run(self, what, cmd, **kw):
        r = self.host.run(cmd, **kw)
        self.log(what, r)
        return r

    def file(self, fid):
        f = self.files.get(fid)
        if f is None:
            raise Reason("error", f"the set has no {SYSTEM} file {fid}")
        if f["sha256"] == "placeholder":
            raise Reason("placeholder_file", fid)
        p = self.cache / f["sha256"]
        if not p.exists():
            raise Reason("error", f"{fid} is not in the cache")
        return p

    def unpack(self, fid):
        """The file unpacked under work, readable by the GUI user."""
        f = self.files[fid]
        src = self.file(fid)
        dest = self.work / fid
        if not dest.exists():
            tmp = self.work / (fid + ".part")
            shutil.rmtree(tmp, ignore_errors=True)
            tmp.mkdir(parents=True)
            if f["kind"] == "zip":
                _unzip(src, tmp)
            else:
                with tarfile.open(src) as t:
                    if hasattr(tarfile, "data_filter"):
                        t.extractall(tmp, filter="tar")
                    else:
                        t.extractall(tmp)
            os.chmod(tmp, 0o755)
            tmp.rename(dest)
        return dest

    def program(self, fid, name):
        """The program called name in the file: its entry, or the one
        executable of that name nearest the top of the archive."""
        f = self.files.get(fid) or {}
        root = self.unpack(fid)
        if f.get("entry"):
            p = root / f["entry"]
            if p.is_file():
                return p
            raise Reason("no_build", f"{fid} holds no {f['entry']}")
        found = sorted((p for p in root.rglob(name) if p.is_file() and os.access(p, os.X_OK)),
                       key=lambda p: (len(p.parts), str(p)))
        if not found:
            raise Reason("no_build", f"{fid} holds no program {name}")
        return found[0]

    def libs(self, fid):
        """LD_LIBRARY_PATH for a program pinned with the libraries it needs."""
        root = self.unpack(fid)
        dirs = sorted({str(p) for p in root.rglob("lib") if p.is_dir()} | {str(p) for p in root.rglob("lib64") if p.is_dir()})
        have = os.environ.get("LD_LIBRARY_PATH")
        return {"LD_LIBRARY_PATH": ":".join(dirs + ([have] if have else []))} if dirs else {}

    def serve(self, fid):
        root = self.unpack(fid) / self.files[fid].get("root", "")
        handler = functools.partial(_Quiet, directory=str(root))
        srv = _Server(("127.0.0.1", 0), handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        return f"http://127.0.0.1:{srv.server_address[1]}/", srv.shutdown


class _Server(http.server.ThreadingHTTPServer):
    # A browser opens many connections at once; the default backlog of 5
    # drops some, and a module that fails to load stops the benchmark.
    request_queue_size = 128
    daemon_threads = True


class _Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *a):
        pass


def _unzip(src, dest):
    with zipfile.ZipFile(src) as z:
        for info in z.infolist():
            path = z.extract(info, dest)
            mode = (info.external_attr >> 16) & 0o777
            if stat.S_ISLNK(info.external_attr >> 16):
                target = pathlib.Path(path).read_text()
                os.unlink(path)
                os.symlink(target, path)
            elif mode:
                os.chmod(path, mode)


def _ok(r, what):
    if r.code != 0:
        raise RuntimeError(f"{what} exited {r.code}")
    return r


@dataclasses.dataclass
class Value:
    value: float
    spread: float = None
    capacity: dict = None


# ---------------------------------------------------------------- CPU

def coremark_program(ctx, threads):
    """The lab's CoreMark build: one program for up to 64 threads, told how
    many by its first argument, M<threads>."""
    return ctx.program("coremark", "coremark")


def run_coremark(ctx, threads, iterations=0):
    exe = coremark_program(ctx, threads)
    r = ctx.run(f"coremark {threads} threads", [exe, f"M{threads}", "0x0", "0x0", "0x66", str(iterations), "7", "1", "2000"],
                timeout=1200, env=ctx.libs("coremark"))
    return _ok(r, "coremark")


def coremark(threads):
    def fn(ctx, m):
        n = threads or ctx.host.cpus()
        return Value(parse_coremark(run_coremark(ctx, n).out))
    return fn


def seven_zip(ctx, m):
    r = _ok(ctx.run("7z b", [ctx.program("7zip", "7z"), "b"], timeout=900), "7z b")
    return Value(parse_7z(r.out))


# ---------------------------------------------------------------- browser

class CDP:
    def __init__(self, ws_url, timeout=30):
        m = re.match(r"ws://([^/:]+):(\d+)(/.*)", ws_url)
        self.sock = socket.create_connection((m.group(1), int(m.group(2))), timeout=timeout)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall((f"GET {m.group(3)} HTTP/1.1\r\nHost: {m.group(1)}:{m.group(2)}\r\n"
                           f"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: {key}\r\n"
                           "Sec-WebSocket-Version: 13\r\n\r\n").encode())
        resp = b""
        while b"\r\n\r\n" not in resp:
            resp += _exact(self.sock, 1)
        if b" 101 " not in resp.split(b"\r\n", 1)[0]:
            raise ConnectionError("no websocket from the browser")
        self.n = 0

    def call(self, method, **params):
        self.n += 1
        self.sock.sendall(ws_frame(json.dumps({"id": self.n, "method": method, "params": params}).encode()))
        while True:
            msg = json.loads(ws_recv(self.sock))
            if msg.get("id") == self.n:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {msg['error']}")
                return msg.get("result", {})

    def eval(self, expr):
        r = self.call("Runtime.evaluate", expression=expr, returnByValue=True, awaitPromise=True)
        if "exceptionDetails" in r:
            return None
        return r.get("result", {}).get("value")

    def close(self):
        self.sock.close()


def ws_frame(payload, mask=True):
    head = bytearray([0x81])
    n = len(payload)
    mbit = 0x80 if mask else 0
    if n < 126:
        head.append(mbit | n)
    elif n < 1 << 16:
        head.append(mbit | 126)
        head += n.to_bytes(2, "big")
    else:
        head.append(mbit | 127)
        head += n.to_bytes(8, "big")
    if not mask:
        return bytes(head) + payload
    key = os.urandom(4)
    return bytes(head) + key + bytes(b ^ key[i % 4] for i, b in enumerate(payload))


def _exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("websocket closed")
        buf += chunk
    return buf


def ws_recv(sock):
    msg = b""
    while True:
        b0, b1 = _exact(sock, 2)
        n = b1 & 0x7F
        if n == 126:
            n = int.from_bytes(_exact(sock, 2), "big")
        elif n == 127:
            n = int.from_bytes(_exact(sock, 8), "big")
        key = _exact(sock, 4) if b1 & 0x80 else None
        data = _exact(sock, n)
        if key:
            data = bytes(b ^ key[i % 4] for i, b in enumerate(data))
        op = b0 & 0x0F
        if op == 0x8:
            raise ConnectionError("websocket closed")
        if op in (0x9, 0xA):
            continue
        msg += data
        if b0 & 0x80:
            return msg


APPS = {
    "speedometer": {
        "url": "index.html?startAutomatically&iterationCount=10",
        "done": "!!(globalThis.benchmarkClient && benchmarkClient._hasResults && !benchmarkClient._isRunning)",
        "result": "(() => { const s = benchmarkClient._metrics.Score;"
                  " return s ? {value: s.mean, spread: s.delta || null} : {value: null}; })()",
        "timeout": 900,
    },
    "motionmark": {
        "url": "MotionMark/index.html",
        "ready": "!!(document.getElementById('start-button') && !document.getElementById('start-button').disabled)",
        "start": "benchmarkController.startBenchmark(); true",
        "done": "document.body.classList.contains('showing-results')",
        "result": "(() => { const r = benchmarkRunnerClient.results;"
                  " return {value: r.score, spread: r.score - r.scoreLowerBound}; })()",
        "timeout": 900,
    },
    "jetstream": {
        "url": "index.html",
        "ready": "!!document.querySelector('#status a.button')",
        "start": "JetStream.start(); true",
        "done": "document.getElementById('result-summary').classList.contains('done')",
        "result": "({value: geomean(JetStream.benchmarks.map(b => b.score)), spread: null})",
        "timeout": 1200,
    },
}

# Headless on the Mac's GPU: a headed window draws only while the compositor
# shows it, and a Lab boot locks the session at once (idle locks it later), so
# a headed page never gets a frame. The screen is the same on every Mac and
# both systems: 1920 by 1080 at a device pixel ratio of 2.
BROWSER_FLAGS = [
    "--no-first-run", "--no-default-browser-check", "--disable-sync", "--disable-extensions",
    "--disable-background-timer-throttling", "--disable-renderer-backgrounding",
    "--headless", "--enable-gpu", "--window-size=1920,1080",
    "--screen-info={0,0 3840x2160 devicePixelRatio=2}",
    "--remote-debugging-port=0", "--remote-allow-origins=*",
]

PROBE = ("(() => { const gl = document.createElement('canvas').getContext('webgl2');"
         " const e = gl && gl.getExtension('WEBGL_debug_renderer_info');"
         " return {renderer: e ? gl.getParameter(e.UNMASKED_RENDERER_WEBGL) : null,"
         " dpr: window.devicePixelRatio}; })()")

FRAME_RATE = ("new Promise(r => { let n = 0; let t0 = null; function f(t) {"
              " if (t0 === null) t0 = t; else n++;"
              " if (t - t0 >= 2000) r(n * 1000 / (t - t0)); else requestAnimationFrame(f); }"
              " requestAnimationFrame(f); })")


def software_gl(renderer):
    """Whether the browser's GL renderer is not the Mac's GPU."""
    return not renderer or bool(re.search(r"swiftshader|llvmpipe|softpipe|software", renderer, re.I))


def browser_verdict(probe, fps):
    """None when the GPU draws the pages at the set's frame rate and pixel
    ratio, else the reason code."""
    if not isinstance(probe, dict) or software_gl(probe.get("renderer")):
        return "software_renderer"
    if probe.get("dpr") != DEVICE_PIXEL_RATIO:
        return "error"
    if not isinstance(fps, (int, float)) or abs(fps - REFRESH_HZ) > REFRESH_HZ * REFRESH_SLACK:
        return "fell_back"
    return None


def _wait(host, fn, timeout, step=2, what="the page"):
    end = host.now() + timeout
    while host.now() < end:
        v = fn()
        if v:
            return v
        host.sleep(step)
    raise TimeoutError(f"{what} did not finish in {timeout} s")


def _try_for(host, fn, seconds, step=1):
    """fn's first truthy value within seconds, else None."""
    end = host.now() + seconds
    while True:
        v = fn()
        if v or host.now() >= end:
            return v
        host.sleep(step)


def _try(f, *a):
    try:
        return f(*a)
    except (OSError, ValueError):
        return None


def _end(proc):
    try:
        proc.wait(10)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except OSError:
            pass
        proc.wait(5)


def browser(app):
    def fn(ctx, m):
        h, a = ctx.host, APPS[app]
        exe = ctx.program("chrome", "chrome")
        base, stop = ctx.serve(app)
        profile = h.gui_dir(ctx.work)
        proc = h.spawn([exe, f"--user-data-dir={profile}", *BROWSER_FLAGS, "about:blank"], gui=True)
        cdp = None
        try:
            port = _wait(h, lambda: _try(devtools_port, profile), 60, 1, "the browser's DevTools port")
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/list", timeout=10) as r:
                pages = [p for p in json.load(r) if p.get("type") == "page"]
            cdp = CDP(pages[0]["webSocketDebuggerUrl"])
            probe = _try_for(h, lambda: (lambda p: p if p and p.get("renderer") else None)(cdp.eval(PROBE)), 30)
            fps = cdp.eval(FRAME_RATE)
            ctx.log(f"{app}: renderer {probe and probe.get('renderer')}, dpr {probe and probe.get('dpr')}, {fps} frames/s")
            why = browser_verdict(probe, fps)
            if why:
                raise Reason(why)
            cdp.call("Page.navigate", url=base + a["url"])
            if "ready" in a:
                _wait(h, lambda: cdp.eval(a["ready"]), 300, 2, "the benchmark's start button")
                cdp.eval(a["start"])
            _wait(h, lambda: cdp.eval(a["done"]), a["timeout"], 5, app)
            value, spread = page_result(cdp.eval(a["result"]))
        finally:
            if cdp:
                try:
                    cdp.call("Browser.close")
                except Exception:
                    pass
                cdp.close()
            stop()
            _end(proc)
            shutil.rmtree(profile, ignore_errors=True)
        return Value(value, spread)
    return fn


# ---------------------------------------------------------------- GPU

def clpeak(test, parse):
    def fn(ctx, m):
        exe = ctx.program("clpeak", "clpeak")
        env = dict(ctx.libs("clpeak"), RUSTICL_ENABLE="asahi")
        r = ctx.run(f"clpeak {test}", [exe, "--opencl", test], timeout=900, env=env, gui=True)
        why = clpeak_verdict(r.code, r.out)
        if why:
            raise Reason(why)
        return Value(parse(r.out))
    return fn


# ---------------------------------------------------------------- Storage

def disk_bytes(host):
    """The internal SSD's size: the biggest whole NVMe disk."""
    sizes = []
    for rel in host.glob("sys/block/nvme*/size"):
        try:
            sizes.append(int(host.read(rel)) * 512)
        except (TypeError, ValueError):
            pass
    return max(sizes) if sizes else None


def storage_room(host, work, file_size):
    """The capacity entry, with the file in place; disk_too_full when less
    than STORAGE_FREE of the disk would stay free."""
    st = host.statvfs(str(work))
    total = st.f_blocks * st.f_frsize
    after = st.f_bavail * st.f_frsize - file_size
    cap = {"bytes": disk_bytes(host), "fill_percent": round(100 * (1 - after / total), 1) if total else None}
    if total <= 0 or after < STORAGE_FREE * total:
        raise Reason("disk_too_full", capacity=cap)
    return cap


def cached_bytes(ctx, path):
    """How much of the file the page cache holds, by fincore; None when it
    cannot tell."""
    exe = ctx.host.which("fincore")
    if not exe:
        return None
    r = ctx.run("fincore", [exe, "--bytes", "--noheadings", "--output", "RES", path], timeout=60)
    try:
        return int(r.out.split()[0]) if r.code == 0 else None
    except (IndexError, ValueError):
        return None


def fio_args(exe, path, s, out):
    a = [exe, f"--name={s['rw']}", f"--filename={path}", f"--rw={s['rw']}", f"--bs={s['bs']}",
         f"--direct={1 if s.get('direct', True) else 0}", f"--ioengine={s.get('ioengine', 'psync')}",
         f"--iodepth={s.get('iodepth', 1)}", "--output-format=json", f"--output={out}"]
    if "write" in s["rw"]:
        a.append("--refill_buffers")
    if s.get("numjobs"):
        a += [f"--numjobs={s['numjobs']}", f"--size={s['range_per_job']}", "--offset=0",
              f"--offset_increment={s['range_per_job']}", "--time_based", f"--runtime={s['runtime_s']}",
              "--norandommap", "--randrepeat=0"]
        if s.get("group_reporting"):
            a.append("--group_reporting")
    else:
        a += ["--numjobs=1", f"--size={s['file']}"]
    return a


def fill_args(exe, path, size, out):
    return [exe, "--name=fill", f"--filename={path}", "--rw=write", "--bs=1M", f"--size={size}", "--direct=1",
            "--ioengine=psync", "--iodepth=1", "--refill_buffers", "--output-format=json", f"--output={out}"]


def fio(ctx, m):
    """One fio test on a scratch file filled before timing, in the work
    folder on the system's disk, removed however the test ends."""
    s = m["settings"]
    exe = ctx.program("fio", "fio")
    env = ctx.libs("fio")
    size = size_bytes(s["file"])
    cap = storage_room(ctx.host, ctx.work, size)
    d = ctx.work / "fio"
    path, out = d / "scratch", d / "fio.json"
    try:
        d.mkdir(exist_ok=True)
        path.touch()
        if ctx.host.which("chattr"):
            ctx.run("chattr", ["chattr", "+m", path], timeout=30)
        _ok(ctx.run("fio fill", fill_args(exe, path, s["file"], d / "fill.json"), timeout=1800, env=env), "fio fill")
        r = ctx.run(f"fio {s['rw']}", fio_args(exe, path, s, out), timeout=1800, env=env)
        try:
            text = out.read_text()
        except OSError:
            text = r.out
        ctx.log("fio json", Ran(r.code, text, r.seconds))
        _ok(r, "fio")
        got = parse_fio(text, s["rw"])
        res = cached_bytes(ctx, path)
        if res is not None and res > CACHED_LIMIT * size:
            raise Reason("fell_back", "direct I/O went through the page cache", capacity=cap)
    except Reason as e:
        e.capacity = e.capacity or cap
        raise
    except Exception as e:
        raise Reason("error", f"{type(e).__name__}: {e}", capacity=cap)
    finally:
        shutil.rmtree(d, ignore_errors=True)
    if m["unit"] == "MB/s":
        return Value(got["mbs"], got["mbs_spread"], cap)
    return Value(got["kiops"], got["kiops_spread"], cap)


# ---------------------------------------------------------------- Memory

def largest_cache(host):
    sizes = []
    for rel in host.glob("sys/devices/system/cpu/cpu*/cache/index*/size"):
        try:
            sizes.append(size_bytes(host.read(rel)))
        except (TypeError, ValueError):
            pass
    return max(sizes + [STREAM_LARGEST_CACHE])


def stream_elements(host):
    """Doubles per array: each array at least STREAM_CACHE_FACTOR times the
    largest cache."""
    return STREAM_CACHE_FACTOR * largest_cache(host) // 8


def stream(kernel):
    def fn(ctx, m):
        exe = ctx.program("stream", "stream")
        env = dict(ctx.libs("stream"), OMP_NUM_THREADS=str(ctx.host.cpus()))
        r = _ok(ctx.run(f"stream {kernel}", [exe, str(stream_elements(ctx.host))], timeout=900, env=env), "stream")
        return Value(parse_stream(r.out, kernel))
    return fn


# ---------------------------------------------------------------- Video

DECODE_HWACCEL = "v4l2request"


def ffmpeg(ctx):
    ff, probe = ctx.host.which("ffmpeg"), ctx.host.which("ffprobe")
    if not ff or not probe:
        raise Reason("no_build", "no ffmpeg or ffprobe on this Mac")
    r = ctx.run("ffmpeg -hwaccels", [ff, "-hide_banner", "-hwaccels"], timeout=60)
    if DECODE_HWACCEL not in r.out.split():
        raise Reason("no_build", f"ffmpeg has no {DECODE_HWACCEL} hwaccel")
    if not ctx.host.glob("dev/media*"):
        raise Reason("no_driver", "no media device")
    return ff, probe


def clip_frames(ctx, probe, clip):
    r = _ok(ctx.run("ffprobe", [probe, "-v", "error", "-select_streams", "v:0", "-count_packets", "-show_entries",
                                "stream=nb_read_packets", "-of", "csv=p=0", clip], timeout=300), "ffprobe")
    try:
        return int(r.out.strip().splitlines()[-1])
    except (IndexError, ValueError):
        raise ParseError("no packet count from ffprobe")


def decode_args(ff, clip, realtime=False):
    return [ff, "-hide_banner", "-nostdin", "-benchmark", *(["-re"] if realtime else []),
            "-hwaccel", DECODE_HWACCEL, "-hwaccel_output_format", "drm_prime", "-i", clip,
            "-map", "0:v:0", "-fps_mode", "passthrough", "-f", "null", "-"]


def hw_decode(ctx, clip_id, realtime=False):
    """Every frame of the clip through the hardware decoder: (frames, frames/s)."""
    ff, probe = ffmpeg(ctx)
    clip = ctx.file(clip_id)
    want = clip_frames(ctx, probe, clip)
    r = ctx.run("ffmpeg decode", decode_args(ff, clip, realtime), timeout=900)
    why = decode_verdict(r.code, r.out)
    if why:
        raise Reason(why)
    got, fps = parse_ffmpeg(r.out)
    if got != want:
        raise Reason("error", f"decoded {got} of {want} frames")
    return got, fps


def decode(clip_id):
    def fn(ctx, m):
        return Value(hw_decode(ctx, clip_id)[1])
    return fn


# ---------------------------------------------------------------- AI

def ane_device(host):
    for rel in host.glob("sys/class/accel/accel*/device/uevent"):
        if re.search(r"^DRIVER=.*ane", host.read(rel) or "", re.M | re.I):
            return True
    return False


def neural_engine(ctx, m):
    model = ctx.file("mobilenet-v1")
    if not ane_device(ctx.host):
        raise Reason("no_driver", "no Neural engine accelerator device")
    tool = ctx.host.which(ANE_TOOL)
    if not tool:
        raise Reason("no_build", f"no {ANE_TOOL} from libane on this Mac")
    s = m["settings"]
    r = _ok(ctx.run("ane-bench", [tool, "--model", model, "--precision", s.get("precision", "fp16"),
                                  "--batch", str(s.get("batch", 1))], timeout=900), "ane-bench")
    dev, v = parse_ane(r.out)
    if dev != "ane":
        raise Reason("fell_back", f"ran on {dev}")
    return Value(v)


def llama(gpu):
    def fn(ctx, m):
        s = m["settings"]
        exe = ctx.program("llama.cpp", "llama-bench")
        model = ctx.file("llama-model")
        p, n = int(s.get("prompt_tokens", 0)), int(s.get("generated_tokens", 0))
        ngl = LLAMA_ALL_LAYERS if gpu else int(s.get("gpu_layers", 0))
        r = ctx.run("llama-bench", [exe, "-m", model, "-p", str(p), "-n", str(n), "-ngl", str(ngl),
                                    "-t", str(ctx.host.cpus()), "-o", "jsonl", "-v"],
                    timeout=1800, env=ctx.libs("llama.cpp"))
        _ok(r, "llama-bench")
        rows = parse_llama(r.out)
        if gpu:
            why = llama_gpu_verdict(r.out, rows)
            if why:
                raise Reason(why)
        row = llama_row(rows, p, n)
        return Value(round(float(row["avg_ts"]), 3), round(float(row.get("stddev_ts") or 0), 3))
    return fn


MLX_PROBE = r"""
import json, sys, time
try:
    import mlx.core as mx
except Exception:
    print(json.dumps({"status": "no_build"})); sys.exit(0)
try:
    ok = mx.is_available(mx.gpu)
except Exception:
    ok = False
if not ok:
    print(json.dumps({"status": "no_build"})); sys.exit(0)
mx.set_default_device(mx.gpu)
n, warmup, reps = (int(a) for a in sys.argv[1:4])
a = mx.random.normal((n, n)).astype(mx.float16)
b = mx.random.normal((n, n)).astype(mx.float16)
mx.eval(a, b)
mx.synchronize()
def step():
    mx.eval(a @ b)
    mx.synchronize()
for _ in range(warmup):
    step()
t = time.perf_counter()
for _ in range(reps):
    step()
dt = time.perf_counter() - t
gpu = "gpu" in str(mx.default_device()).lower()
print(json.dumps({"status": "ok" if gpu else "fell_back", "tflops": 2 * n ** 3 * reps / dt / 1e12}))
"""
# As tools/bench-macos/mlx-bench times it, so both systems time the same work.
MLX_SIZE, MLX_WARMUP, MLX_REPS = 4096, 5, 50


def mlx(ctx, m):
    py = ctx.host.which("python3")
    if not py:
        raise Reason("no_build", "no python3")
    r = _ok(ctx.run("mlx", [py, "-c", MLX_PROBE, str(MLX_SIZE), str(MLX_WARMUP), str(MLX_REPS)], timeout=900), "mlx")
    got = parse_mlx(r.out)
    if got.get("status") in ("no_build", "fell_back"):
        raise Reason(got["status"])
    if got.get("status") != "ok" or not isinstance(got.get("tflops"), (int, float)):
        raise ParseError("the MLX probe gave no TFLOPS")
    return Value(round(got["tflops"], 3))


# ---------------------------------------------------------------- Power

def pstr(host):
    """macsmc-hwmon's input for the SMC's whole-Mac power key, PSTR."""
    for rel in host.glob("sys/class/hwmon/hwmon*/name"):
        if "macsmc" not in (host.read(rel) or ""):
            continue
        for lbl in host.glob(os.path.dirname(rel) + "/power*_label"):
            label = (host.read(lbl) or "").strip()
            if label == "PSTR" or label.lower() == "total system power":
                return lbl[: -len("_label")] + "_input"
    return None


def watts(host, rel):
    try:
        w = int(host.read(rel)) / 1e6
    except (TypeError, ValueError):
        return None
    return w if 0 < w < POWER_MAX_W else None


def power_sensor(ctx):
    rel = pstr(ctx.host)
    if not rel or watts(ctx.host, rel) is None:
        raise Reason("no_comparable_sensor")
    if battery_charging(ctx.host):
        raise Reason("battery_charging")
    return rel


def _enough(samples, seconds):
    return len(samples) >= max(1, math.floor(0.5 * seconds))


def power_idle(ctx, m):
    rel = power_sensor(ctx)
    secs = int(m["settings"].get("seconds", IDLE_SECONDS))
    got = []
    for _ in range(secs):
        ctx.host.sleep(ctx.power_interval)
        w = watts(ctx.host, rel)
        if w is not None:
            got.append(w)
    if battery_charging(ctx.host):
        raise Reason("battery_charging")
    if not _enough(got, secs):
        raise Reason("no_comparable_sensor", f"{len(got)} valid samples of {secs}")
    ctx.log(f"power idle: {len(got)} samples")
    return Value(round(statistics.fmean(got), 3), round(statistics.pstdev(got), 3))


def energy(ctx, rel, work):
    """Joules the work takes: the mean of PSTR, sampled every
    power_interval while it runs, times how long it ran."""
    got, stop = [], threading.Event()

    def sample():
        while not stop.wait(ctx.power_interval):
            w = watts(ctx.host, rel)
            if w is not None:
                got.append(w)
    t = threading.Thread(target=sample, daemon=True)
    start = time.monotonic()
    t.start()
    try:
        work()
    finally:
        stop.set()
        t.join()
    secs = time.monotonic() - start
    if battery_charging(ctx.host):
        raise Reason("battery_charging")
    if not _enough(got, secs / ctx.power_interval):
        raise Reason("no_comparable_sensor", f"{len(got)} valid samples in {secs:.0f} s")
    ctx.log(f"power: {len(got)} samples in {secs:.1f} s")
    return Value(round(statistics.fmean(got) * secs, 3), None)


def power_playback(ctx, m):
    rel = power_sensor(ctx)
    return energy(ctx, rel, lambda: hw_decode(ctx, "clip-h264-1080p", realtime=True))


def power_all_core(ctx, m):
    rel = power_sensor(ctx)
    n = ctx.host.cpus()
    per = -(-POWER_COREMARK_ITERATIONS // n)

    def work():
        coremark_valid(run_coremark(ctx, n, per).out)
    return energy(ctx, rel, work)


# ---------------------------------------------------------------- Omarchy only

def boot_time(ctx, m):
    r = _ok(ctx.run("systemd-analyze", ["systemd-analyze"], timeout=60), "systemd-analyze")
    return Value(parse_boot(r.out))


METRICS = {
    "coremark-single": coremark(1),
    "coremark-all": coremark(None),
    "7zip": seven_zip,
    "motionmark": browser("motionmark"),
    "clpeak-global-bandwidth": clpeak("--global-memory-bandwidth", parse_clpeak_bandwidth),
    "clpeak-fp32": clpeak("--single-precision-compute", parse_clpeak_fp32),
    "fio-seq-read": fio,
    "fio-seq-write": fio,
    "fio-rand-read-4k": fio,
    "fio-rand-write-4k": fio,
    "stream-triad": stream("Triad"),
    "stream-copy": stream("Copy"),
    "speedometer": browser("speedometer"),
    "jetstream": browser("jetstream"),
    "decode-h264-1080p": decode("clip-h264-1080p"),
    "decode-hevc-4k": decode("clip-hevc-4k"),
    "neural-engine-mobilenet-v1": neural_engine,
    "llama-prompt-gpu": llama(gpu=True),
    "llama-gen-gpu": llama(gpu=True),
    "llama-gen-cpu": llama(gpu=False),
    "mlx-matmul-fp16": mlx,
    "power-idle": power_idle,
    "power-playback-1080p": power_playback,
    "power-all-core": power_all_core,
    "boot-time": boot_time,
}


# ---------------------------------------------------------------- the list

def entry(bset, m, value=None, spread=None, seconds=0.0, cool=None, capacity=None, reason=None):
    side = m.get(SYSTEM) or {}
    return {
        "set": bset["set"], "metric": m["id"], "category": m["category"], "version": m["version"],
        "value": value, "unit": m["unit"], "higher_is_better": m["direction"] == "higher",
        "spread": spread, "seconds": round(seconds, 3), "cool_down": cool, "api": side.get("api"),
        "capacity": capacity, "reason": reason,
    }


def _number(x):
    return isinstance(x, (int, float)) and not isinstance(x, bool) and math.isfinite(x)


def clean(e):
    """The entry as bench.json may hold it: FIELDS only, a value or a
    reason, never both, and numbers that are numbers."""
    e = {k: e.get(k) for k in FIELDS}
    if e["reason"] not in REASONS + (None,):
        e["reason"] = "error"
    if e["reason"] is None and not _number(e["value"]):
        e["reason"] = "error"
    if e["reason"] is not None:
        e["value"] = e["spread"] = None
    if e["spread"] is not None and not _number(e["spread"]):
        e["spread"] = None
    cd = e["cool_down"]
    cd = cd.get("seconds") if isinstance(cd, dict) else cd
    e["cool_down"] = float(cd) if _number(cd) else 0.0
    cap = e["capacity"]
    e["capacity"] = e["fill"] = None
    if isinstance(cap, dict) and _number(cap.get("bytes")) and cap["bytes"] > 0:
        e["capacity"] = max(1, round(cap["bytes"] / 1e9))
        if _number(cap.get("fill_percent")):
            e["fill"] = cap["fill_percent"]
    return e


def selected(bset, lst, only=None):
    """The list's metrics on this system, in the set's order; with only, just
    those metric ids, each of which must be in the list."""
    ms = [m for m in bset["metrics"] if m.get(SYSTEM) is not None and (lst == "full" or m["list"] == lst)]
    if only is None:
        return ms
    have = {m["id"] for m in ms}
    unknown = [i for i in only if i not in have]
    if unknown:
        raise ValueError(f"not in {bset['set']}'s {lst} list on {SYSTEM}: {', '.join(unknown)}")
    if not only or len(set(only)) != len(only):
        raise ValueError("no metric, or one given twice")
    return [m for m in ms if m["id"] in only]


def placeholder(ctx, m):
    for fid in m[SYSTEM].get("files", []):
        f = ctx.files.get(fid)
        if f is None or f["sha256"] == "placeholder":
            return fid
    return None


def run_list(metrics, ctx, cool=None, impl=None, out=None):
    """Each metric once, in order, into out: a cancelled run keeps what it
    measured before."""
    impl = METRICS if impl is None else impl
    h = ctx.host
    out = [] if out is None else out
    for m in metrics:
        start = h.now()
        cd = None
        try:
            fn = impl.get(m["id"])
            if fn is None:
                raise Reason("not_run", "this runner has no such metric")
            if placeholder(ctx, m):
                raise Reason("placeholder_file", placeholder(ctx, m))
            if cool:
                w, t = cool(h)
                cd = {"seconds": w, "temp_c": t}
            start = h.now()
            ctx.log(f"{m['id']}: start")
            v = fn(ctx, m)
            e = entry(ctx.set, m, v.value, v.spread, h.now() - start, cd, v.capacity)
        except Reason as r:
            ctx.log(f"{m['id']}: {r}")
            e = entry(ctx.set, m, seconds=h.now() - start, cool=cd, capacity=r.capacity, reason=r.code)
        except Exception:
            ctx.log(f"{m['id']}: error\n{traceback.format_exc()}")
            e = entry(ctx.set, m, seconds=h.now() - start, cool=cd, reason="error")
        out.append(clean(e))
    return out


def exit_code(results):
    return 1 if any(r["reason"] in FAILING for r in results) else 0


def _cancel(*_):
    raise Cancelled()


def main(argv=None, host=None, stderr=sys.stderr, impl=None):
    ap = argparse.ArgumentParser(prog="bench.py")
    ap.add_argument("--metrics", action="store_true", help="print the metric ids this runner runs and stop")
    ap.add_argument("--reasons", action="store_true", help="print the reason codes and stop")
    ap.add_argument("--set")
    ap.add_argument("--list", choices=LISTS, default="quick")
    ap.add_argument("--only", help="a Performance run's metric ids, comma-separated: only those of the list run")
    ap.add_argument("--cache")
    ap.add_argument("--work")
    ap.add_argument("--out")
    ap.add_argument("--gui-user")
    ap.add_argument("--gui-env-file")
    a = ap.parse_args(argv)
    if a.metrics:
        print("\n".join(sorted(METRICS)))
        return 0
    if a.reasons:
        print("\n".join(REASONS))
        return 0
    if host is None:
        env = []
        if a.gui_env_file:
            env = [l for l in pathlib.Path(a.gui_env_file).read_text().split("\n") if "=" in l]
        host = Host("/", a.gui_user, env)
    bset = json.loads(pathlib.Path(a.set).read_text())
    try:
        metrics = selected(bset, a.list, None if a.only is None else a.only.split(","))
    except ValueError as e:
        print(f"bench.py: --only: {e}", file=stderr)
        return 2
    out = pathlib.Path(a.out)
    cond_path = out.parent / "bench-conditions.json"
    cond = {"set": bset["set"], "list": a.list, "refused": None}
    results, saved, lock = [], None, None
    old = signal.signal(signal.SIGTERM, _cancel) if threading.current_thread() is threading.main_thread() else None
    try:
        lock = host.lock() if host.root == "/" else None
        _, saved = conditions(host, c=cond)
        print(f"conditions: {json.dumps(cond)}", file=stderr, flush=True)
        ctx = Ctx(host, bset, a.cache, a.work, log=stderr)
        run_list(metrics, ctx, cool=cool_down, impl=impl, out=results)
        code = exit_code(results)
    except Refused as e:
        cond["refused"] = e.code
        print(f"refused: {e}", file=stderr)
        code = 3
    except Cancelled:
        print("cancelled", file=stderr)
        code = 143
    finally:
        restore(host, saved)
        out.write_text(json.dumps(results, indent=2) + "\n")
        cond_path.write_text(json.dumps(cond, indent=2) + "\n")
        if lock:
            lock.close()
        if old is not None:
            signal.signal(signal.SIGTERM, old)
    for r in results:
        print(json.dumps(r), file=stderr)
    return code


if __name__ == "__main__":
    sys.exit(main())
