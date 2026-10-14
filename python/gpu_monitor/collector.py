"""GPU sampler -> InfluxDB line protocol.

Per tick (nanosecond timestamps, the engine's native precision):

* one ``gpu`` point per GPU with memory, utilisation, temperature and power
  fields;
* one ``gpu_proc`` point per process holding a context on the GPU, tagged by
  pid / process name / kind (``cuda`` or ``gfx``), with its dedicated GPU
  memory.

Windows (WDDM) does not report per-process allocations through NVML
(``usedGpuMemory`` is None), so dedicated memory per process is streamed from
a persistent PowerShell ``Get-Counter -Continuous`` child (the Task Manager
data source). Raw PDH calls return PDH_NO_DATA for this counter set on
current drivers, while Get-Counter works, so it is used as the provider.

Optional metrics (temperature, power, process list) are skipped when the
driver does not report them; a failed mandatory read skips that GPU for the
tick and records the reason in :attr:`GPUSampler.last_error`.
"""
from __future__ import annotations

import re
import subprocess
import threading
import time

import pynvml

MIB = 1024 * 1024


def _text(value) -> str:
    """NVML returns str or bytes depending on the binding version."""
    if isinstance(value, bytes):
        return value.decode(errors="replace")
    return str(value)


def escape_tag(value: str) -> str:
    """Escape a line-protocol tag value (backslash, comma, space, equals)."""
    return (value.replace("\\", "\\\\")
                 .replace("=", "\\=")
                 .replace(",", "\\,")
                 .replace(" ", "\\ "))


class _WindowsGpuProcMemory:
    """Per-process dedicated GPU memory from a streaming Get-Counter child.

    The child prints one ``instance=value`` line per counter instance every
    ``interval`` seconds; a daemon thread folds the stream into a
    ``{pid: bytes}`` map (largest value when a pid spans several GPUs).
    """

    CMD = ("Get-Counter '\\GPU Process Memory(*)\\Dedicated Usage' "
           "-Continuous -SampleInterval {interval} | "
           "ForEach-Object {{ $_.CounterSamples | ForEach-Object {{ "
           "'{{0}}={{1}}' -f $_.InstanceName, $_.CookedValue }} }}")

    _PID_RE = re.compile(r"^pid_(\d+)_")

    def __init__(self, interval: int = 2) -> None:
        self._mem: dict[int, int] = {}
        self._proc = subprocess.Popen(
            ["powershell", "-NoProfile", "-Command",
             self.CMD.format(interval=interval)],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        self._pump = threading.Thread(target=self._read_loop, daemon=True)
        self._pump.start()

    def _read_loop(self) -> None:
        try:
            for line in self._proc.stdout:
                name, sep, value = line.strip().rpartition("=")
                if not sep:
                    continue
                match = self._PID_RE.match(name)
                if not match:
                    continue
                try:
                    mem = int(float(value))
                except ValueError:
                    continue
                pid = int(match.group(1))
                self._mem[pid] = max(self._mem.get(pid, 0), mem)
        except (OSError, ValueError):
            pass

    def snapshot(self) -> dict[int, int]:
        """Current {pid: dedicated bytes} (empty until the first sample)."""
        return dict(self._mem)

    def close(self) -> None:
        try:
            self._proc.kill()
        except OSError:
            pass


class GPUSampler:
    """Reads every GPU once per :meth:`sample` call as line protocol."""

    def __init__(self) -> None:
        pynvml.nvmlInit()
        self.driver = _text(pynvml.nvmlSystemGetDriverVersion())
        try:
            cuda = int(pynvml.nvmlSystemGetCudaDriverVersion())  # e.g. 13020
            self.cuda = "{}.{}".format(cuda // 1000, (cuda % 1000) // 10)
        except Exception:
            self.cuda = "?"
        self.count = pynvml.nvmlDeviceGetCount()
        self.handles = [pynvml.nvmlDeviceGetHandleByIndex(i)
                        for i in range(self.count)]
        self.names = [_text(pynvml.nvmlDeviceGetName(h)) for h in self.handles]
        self.last_error = ""
        try:
            self._winmem = _WindowsGpuProcMemory()
        except OSError:
            self._winmem = None  # counter stream unavailable: mem = 0

    # -- sampling ----------------------------------------------------------- #

    def sample(self) -> str:
        """Return one tick of line-protocol text ('' when nothing read)."""
        ts = time.time_ns()
        lines: list[str] = []
        for i, handle in enumerate(self.handles):
            try:
                lines.append(self._gpu_line(i, handle, ts))
                lines.extend(self._proc_lines(i, handle, ts))
                self.last_error = ""
            except pynvml.NVMLError as exc:
                self.last_error = "gpu{}: {}".format(i, exc)
        if not lines:
            return ""
        return "\n".join(lines) + "\n"

    def _gpu_line(self, index: int, handle, ts: int) -> str:
        mem = pynvml.nvmlDeviceGetMemoryInfo(handle)
        util = pynvml.nvmlDeviceGetUtilizationRates(handle)
        fields = [
            "mem_used={}i".format(mem.used // MIB),
            "mem_total={}i".format(mem.total // MIB),
            "util_gpu={}i".format(util.gpu),
            "util_mem={}i".format(util.memory),
        ]
        try:
            temp = pynvml.nvmlDeviceGetTemperature(
                handle, pynvml.NVML_TEMPERATURE_GPU)
            fields.append("temp={}i".format(temp))
        except pynvml.NVMLError:
            pass
        try:
            fields.append("power={:.1f}".format(
                pynvml.nvmlDeviceGetPowerUsage(handle) / 1000.0))
        except pynvml.NVMLError:
            pass
        return "gpu,gpu={} {} {}".format(index, ",".join(fields), ts)

    def _proc_lines(self, index: int, handle, ts: int) -> list[str]:
        try:
            compute = list(pynvml.nvmlDeviceGetComputeRunningProcesses(handle))
            graphics = list(pynvml.nvmlDeviceGetGraphicsRunningProcesses(handle))
        except pynvml.NVMLError:
            return []
        mem_bytes = self._winmem.snapshot() if self._winmem else {}

        # All CUDA contexts are listed; graphics contexts only when they
        # actually hold dedicated memory (keeps desktop noise out).
        pids: dict[int, str] = {info.pid: "cuda" for info in compute}
        for info in graphics:
            if info.pid not in pids and mem_bytes.get(info.pid, 0) > 0:
                pids[info.pid] = "gfx"

        lines = []
        for pid, kind in sorted(pids.items()):
            mem = mem_bytes.get(pid, 0) // MIB
            lines.append(
                "gpu_proc,gpu={},pid={},kind={},proc={} mem={}i {}".format(
                    index, pid, kind, escape_tag(self._proc_name(pid)),
                    mem, ts))
        return lines

    @staticmethod
    def _proc_name(pid: int) -> str:
        try:
            name = _text(pynvml.nvmlSystemGetProcessName(pid))
        except pynvml.NVMLError:
            return "pid:{}".format(pid)
        name = name.replace("\\", "/")
        return name.rsplit("/", 1)[-1] or name

    def shutdown(self) -> None:
        if self._winmem is not None:
            self._winmem.close()
        try:
            pynvml.nvmlShutdown()
        except pynvml.NVMLError:
            pass
