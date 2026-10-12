#!/usr/bin/python3
"""Reload HP host-based firmware after power cycles or hotplug in the AIO image.

Inspired by tzishue/cloud-printer's fix-foo2zjs-printer.sh. Use the existing CUPS
USB backend for device IDs and transfers, so no usblp nodes or pyusb are needed.
"""

import argparse
from collections import Counter
from dataclasses import dataclass
import fcntl
import logging
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import threading
import time
from urllib.parse import unquote, urlsplit


# USB product IDs from foo2zjs hplj1000; M1005 MFP is deliberately excluded.
PRODUCTS = {
    "0517": "1000", "1317": "1005", "4117": "1018", "2b17": "1020",
    "3d17": "P1005", "3e17": "P1006", "4817": "P1007",
    "4917": "P1008", "3f17": "P1505",
}
MODELS = {model: model for model in PRODUCTS.values()}
MODELS.update({"1020 PLUS": "1020", "P1007": "P1005", "P1008": "P1006"})
MODEL_RE = re.compile(r"^(?:HP\s+)?LASERJET\s+(.+)$", re.I)
LOG = logging.getLogger("hp-firmware")


@dataclass(frozen=True)
class Printer:
    uri: str
    firmware: str
    loaded: bool


def firmware_model(model):
    match = MODEL_RE.fullmatch(" ".join(model.upper().split()))
    return MODELS.get(match[1]) if match else None


def parse_printer(line):
    """Require an HP model in both the URI and a readable IEEE-1284 ID."""
    try:
        fields = shlex.split(line)
        if len(fields) < 5 or fields[0] != "direct":
            return None
        uri = urlsplit(fields[1])
        if uri.scheme != "usb" or uri.hostname.lower() not in ("hp", "hewlett-packard"):
            return None
        device_id = fields[4]
        if not device_id.endswith(";"):
            return None
        attrs = {}
        for part in device_id.split(";"):
            key, sep, value = part.partition(":")
            if sep:
                attrs[key.strip().upper()] = value.strip()
        manufacturer = attrs.get("MFG", attrs.get("MANUFACTURER", ""))
        if re.sub(r"[\s-]", "", manufacturer.lower()) not in ("hp", "hewlettpackard"):
            return None
        model = firmware_model(attrs.get("MDL", attrs.get("MODEL", "")))
        if not model or model != firmware_model(unquote(uri.path.lstrip("/"))):
            return None
        loaded = "FWVER" in attrs or "FIRMWARE" in attrs
        return Printer(fields[1], f"sihp{model}.dl", loaded)
    except (ValueError, AttributeError):
        return None


class FirmwareMonitor:
    def __init__(self, backend="/usr/lib/cups/backend/usb",
                 firmware_dir="/lib/firmware/hp", sysfs_root="/sys/bus/usb/devices"):
        self.backend = backend
        self.firmware_dir = Path(firmware_dir)
        self.sysfs_root = Path(sysfs_root)
        self.last_attempt = {}
        self.messages = {}

    def report(self, key, message, level=logging.INFO):
        # Stable states (missing firmware, busy device, etc.) should not flood logs.
        if self.messages.get(key) != message:
            LOG.log(level, "%s: %s", key, message)
            self.messages[key] = message

    def prevent_autosuspend(self):
        try:
            devices = list(self.sysfs_root.iterdir())
        except OSError:
            # If sysfs is unavailable, let libusb discovery decide instead.
            return True
        found = False
        for device in devices:
            try:
                if device.joinpath("idVendor").read_text().strip().lower() != "03f0":
                    continue
                if device.joinpath("idProduct").read_text().strip().lower() not in PRODUCTS:
                    continue
                found = True
                control = device / "power/control"
                if control.read_text().strip() != "on":
                    control.write_text("on\n")
                    LOG.info("%s: disabled USB autosuspend", device.name)
            except OSError:
                # /sys can be read-only; firmware loading still works via libusb.
                continue
        return found

    def run_backend(self, args, timeout, uri=None):
        env = dict(os.environ, LC_ALL="C")
        env.pop("DEVICE_URI", None)
        if uri is not None:
            env["DEVICE_URI"] = uri
        result = subprocess.run([self.backend, *args], env=env, timeout=timeout,
                                capture_output=True, text=True, errors="replace")
        if result.returncode:
            raise RuntimeError(f"USB backend exited {result.returncode}: {result.stderr[-1000:].strip()}")
        return result.stdout

    def check(self):
        if not self.prevent_autosuspend():
            # Avoid repeatedly opening unrelated USB printers on other systems.
            self.last_attempt.clear()
            self.messages.clear()
            return True
        try:
            output = self.run_backend([], timeout=20)
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
            self.report("discovery", str(error), logging.WARNING)
            return False
        self.messages.pop("discovery", None)

        printers = [printer for line in output.splitlines()
                    if (printer := parse_printer(line)) is not None]
        counts = Counter(printer.uri for printer in printers)
        self.last_attempt = {uri: stamp for uri, stamp in self.last_attempt.items() if uri in counts}
        self.messages = {uri: message for uri, message in self.messages.items() if uri in counts}
        success = True
        for printer in printers:
            uri = printer.uri
            if counts[uri] != 1:
                self.report(uri, "ambiguous USB URI; skipping firmware upload", logging.WARNING)
                success = False
                continue
            if printer.loaded:
                self.report(uri, "firmware already loaded")
                self.last_attempt.pop(uri, None)
                continue
            firmware = self.firmware_dir / printer.firmware
            try:
                present = firmware.is_file() and firmware.stat().st_size > 0
            except OSError:
                present = False
            if not present:
                self.report(uri, f"missing firmware {firmware}; install the matching HP firmware in driver management",
                            logging.WARNING)
                success = False
                continue
            now = time.monotonic()
            if now - self.last_attempt.get(uri, float("-inf")) < 30:
                continue
            self.last_attempt[uri] = now
            try:
                # Match the upstream loader invocation, with a hard timeout and
                # the exact discovered URI (including its serial/interface).
                self.run_backend(["1", "root", "HP firmware", "1", "", str(firmware)],
                                 timeout=30, uri=uri)
                self.report(uri, f"firmware sent: {firmware.name}; waiting for FWVER confirmation")
            except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
                self.report(uri, f"firmware upload failed: {error}", logging.WARNING)
                success = False
        return success


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--once", action="store_true", help="check once instead of monitoring")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s [hp-firmware] %(levelname)s %(message)s")
    # Also prevent manual invocations from racing the background uploader.
    with open("/run/hp-firmware-monitor.lock", "w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            LOG.info("firmware monitor is already running")
            return 0
        monitor = FirmwareMonitor()
        if args.once:
            return 0 if monitor.check() else 1
        stop = threading.Event()
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, lambda *_: stop.set())
        LOG.info("monitor started; checking HP host-based printers every 15 seconds")
        # Allow USB enumeration to settle, as the upstream hplj loaders do.
        if stop.wait(3):
            return 0
        while not stop.is_set():
            monitor.check()
            stop.wait(15)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
