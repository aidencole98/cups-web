"""Firmware recovery regression tests; no USB devices or system paths are touched."""

import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location(
    "hp_firmware_monitor", Path(__file__).with_name("hp-firmware-monitor.py"))
monitor = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = monitor
SPEC.loader.exec_module(monitor)


def device_line(model="LaserJet 1020", serial="A", extra="", uri=None, device_id=None):
    if uri is None:
        uri = f"usb://HP/{model.replace(' ', '%20')}?serial={serial}"
    if device_id is None:
        device_id = f"MFG:Hewlett-Packard;MDL:{model};{extra}"
    return f'direct {uri} "HP {model}" "HP {model}" "{device_id}" ""'


class ParseTests(unittest.TestCase):
    def test_supported_models_and_aliases(self):
        for model, firmware in (
            ("1020", "1020"), ("1020 Plus", "1020"), ("1000", "1000"),
            ("1005", "1005"), ("1018", "1018"), ("P1005", "P1005"),
            ("P1006", "P1006"), ("P1007", "P1005"), ("P1008", "P1006"),
            ("P1505", "P1505"),
        ):
            with self.subTest(model=model):
                printer = monitor.parse_printer(device_line(f"LaserJet {model}"))
                self.assertEqual(printer.firmware, f"sihp{firmware}.dl")
                self.assertFalse(printer.loaded)

    def test_firmware_state_comes_from_id_not_uri_or_description(self):
        self.assertFalse(monitor.parse_printer(device_line(serial="FWVER_1020")).loaded)
        for field in ("FWVER:20200101;", "FIRMWARE:ready;", "fwver:;"):
            with self.subTest(field=field):
                self.assertTrue(monitor.parse_printer(device_line(extra=field)).loaded)

    def test_unknown_unsupported_and_mismatched_devices_are_skipped(self):
        for line in (
            device_line("LaserJet M1005"), device_line("LaserJet 10200"),
            device_line(device_id=""), device_line(device_id="MFG:HP;"),
            device_line(device_id="MFG:HP;MDL:LaserJet 1020"),
            device_line(device_id="MFG:Canon;MDL:LaserJet 1020;"),
            device_line(device_id="MFG:HP;MDL:LaserJet P1006;"),
            device_line(uri="usb://Canon/LaserJet%201020?serial=A"),
            device_line(uri="ipp://HP/LaserJet%201020"),
            device_line(uri="usb:///LaserJet%201020"),
            'direct usb://HP/ "unterminated', "",
        ):
            with self.subTest(line=line):
                self.assertIsNone(monitor.parse_printer(line))


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.firmware = self.root / "firmware"
        self.firmware.mkdir()
        self.sysfs = self.root / "usb"
        self.sysfs.mkdir()
        self.backend = self.root / "backend"
        # A real subprocess validates backend argv, environment and exit handling.
        self.backend.write_text(f"#!{sys.executable}\n" + '''
import json, os, pathlib, sys, time
root = pathlib.Path(__file__).parent
spec = json.loads((root / "spec.json").read_text())
with (root / "calls.jsonl").open("a") as calls:
    calls.write(json.dumps({"args": sys.argv[1:], "uri": os.getenv("DEVICE_URI")}) + "\\n")
time.sleep(spec.get("delay", 0))
if len(sys.argv) == 1:
    print(spec["devices"])
    sys.exit(spec.get("discovery_exit", 0))
sys.stderr.write("simulated upload error" if spec.get("upload_exit", 0) else "")
sys.exit(spec.get("upload_exit", 0))
''')
        self.backend.chmod(0o755)
        self.worker = monitor.FirmwareMonitor(self.backend, self.firmware, self.sysfs)
        self.usb_device("fixture", "03f0", "2b17")
        self.set_devices()

    def set_devices(self, *lines, **options):
        (self.root / "spec.json").write_text(json.dumps({"devices": "\n".join(lines), **options}))

    def install(self, name="sihp1020.dl"):
        (self.firmware / name).write_bytes(b"test firmware")

    def uploads(self):
        calls = self.root / "calls.jsonl"
        if not calls.exists():
            return []
        return [call for line in calls.read_text().splitlines()
                if (call := json.loads(line))["args"]]

    def usb_device(self, name, vendor, product):
        device = self.sysfs / name
        (device / "power").mkdir(parents=True)
        (device / "idVendor").write_text(vendor)
        (device / "idProduct").write_text(product)
        (device / "power/control").write_text("auto\n")
        return device / "power/control"

    def test_two_models_use_correct_firmware_and_exact_serial(self):
        self.install()
        self.install("sihpP1005.dl")
        self.set_devices(device_line(serial="A"), device_line("LaserJet P1007", serial="B"),
                         device_line(serial="C", extra="FWVER:ready;"))
        self.assertTrue(self.worker.check())
        uploads = self.uploads()
        self.assertEqual(len(uploads), 2)
        self.assertTrue(uploads[0]["uri"].endswith("serial=A"))
        self.assertTrue(uploads[1]["uri"].endswith("serial=B"))
        self.assertEqual(uploads[0]["args"], ["1", "root", "HP firmware", "1", "",
                                           str(self.firmware / "sihp1020.dl")])
        self.assertEqual(uploads[1]["args"][-1], str(self.firmware / "sihpP1005.dl"))

    def test_installing_firmware_after_startup_is_detected(self):
        self.set_devices(device_line())
        self.assertFalse(self.worker.check())
        self.assertEqual(self.uploads(), [])
        self.install()
        self.assertTrue(self.worker.check())
        self.assertEqual(len(self.uploads()), 1)

    def test_power_cycle_of_same_serial_reloads_after_loaded_state(self):
        self.install()
        self.set_devices(device_line())
        with patch.object(monitor.time, "monotonic", return_value=100):
            self.worker.check()
            self.set_devices(device_line(extra="FWVER:ready;"))
            self.worker.check()
            self.worker.check()
            self.assertEqual(len(self.uploads()), 1)
            self.set_devices(device_line())
            self.worker.check()
        self.assertEqual(len(self.uploads()), 2)

    def test_hotplug_clears_old_attempt_state(self):
        self.install()
        self.set_devices(device_line())
        with patch.object(monitor.time, "monotonic", return_value=100):
            self.worker.check()
            self.set_devices()
            self.worker.check()
            self.set_devices(device_line())
            self.worker.check()
        self.assertEqual(len(self.uploads()), 2)

    def test_failure_retries_with_cooldown_and_does_not_block_other_device(self):
        self.install()
        self.set_devices(device_line(serial="A"), device_line(serial="B"), upload_exit=1)
        with patch.object(monitor.time, "monotonic", return_value=100):
            self.assertFalse(self.worker.check())
            self.assertEqual(len(self.uploads()), 2)
            self.worker.check()
            self.assertEqual(len(self.uploads()), 2)
        self.set_devices(device_line(serial="A"), device_line(serial="B"))
        with patch.object(monitor.time, "monotonic", return_value=131):
            self.assertTrue(self.worker.check())
        self.assertEqual(len(self.uploads()), 4)

    def test_loaded_unknown_and_duplicate_uri_never_receive_firmware(self):
        self.install()
        self.set_devices(device_line(extra="FWVER:ready;"), device_line(device_id=""),
                         device_line(serial="B"), device_line(serial="B"))
        self.worker.check()
        self.assertEqual(self.uploads(), [])

    def test_autosuspend_only_changes_supported_hp_devices(self):
        hp = self.usb_device("1-1", "03f0", "2b17")
        mfp = self.usb_device("1-2", "03f0", "3b17")
        canon = self.usb_device("1-3", "04a9", "2b17")
        self.worker.check()
        self.assertEqual(hp.read_text(), "on\n")
        self.assertEqual(mfp.read_text(), "auto\n")
        self.assertEqual(canon.read_text(), "auto\n")

    def test_readonly_sysfs_does_not_prevent_upload(self):
        self.usb_device("1-1", "03f0", "2b17")
        self.install()
        self.set_devices(device_line())
        with patch.object(Path, "write_text", side_effect=PermissionError("read-only sysfs")):
            self.assertTrue(self.worker.check())
        self.assertEqual(len(self.uploads()), 1)

    def test_no_supported_hp_skips_backend_discovery(self):
        (self.sysfs / "fixture/idProduct").write_text("3b17")
        self.set_devices(discovery_exit=1)
        self.assertTrue(self.worker.check())
        self.assertFalse((self.root / "calls.jsonl").exists())

    def test_backend_timeout_is_bounded_and_discovery_failure_can_recover(self):
        self.set_devices(delay=2)
        with self.assertRaises(subprocess.TimeoutExpired):
            self.worker.run_backend([], timeout=0.1)
        self.set_devices(discovery_exit=1)
        self.assertFalse(self.worker.check())
        self.set_devices()
        self.assertTrue(self.worker.check())


if __name__ == "__main__":
    unittest.main()
