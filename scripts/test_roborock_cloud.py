import importlib.util
import sys
import types
import unittest
from pathlib import Path


class _FakeB01Q10DP:
    START_CLEAN = "START_CLEAN"
    RESUME = "RESUME"
    PAUSE = "PAUSE"
    START_DOCK_TASK = "START_DOCK_TASK"
    STOP = "STOP"


def _install_roborock_stubs() -> None:
    modules = {
        "roborock": types.ModuleType("roborock"),
        "roborock.data": types.ModuleType("roborock.data"),
        "roborock.data.b01_q10": types.ModuleType("roborock.data.b01_q10"),
        "roborock.data.b01_q10.b01_q10_code_mappings": types.ModuleType(
            "roborock.data.b01_q10.b01_q10_code_mappings"
        ),
        "roborock.data.containers": types.ModuleType("roborock.data.containers"),
        "roborock.devices": types.ModuleType("roborock.devices"),
        "roborock.devices.device_manager": types.ModuleType("roborock.devices.device_manager"),
        "roborock.web_api": types.ModuleType("roborock.web_api"),
    }
    modules["roborock.data.b01_q10.b01_q10_code_mappings"].B01_Q10_DP = _FakeB01Q10DP
    modules["roborock.data.containers"].UserData = type(
        "UserData",
        (),
        {"from_dict": staticmethod(lambda data: data), "as_dict": lambda self: {}},
    )
    modules["roborock.devices.device_manager"].UserParams = type("UserParams", (), {})
    modules["roborock.devices.device_manager"].create_device_manager = None
    modules["roborock.web_api"].RoborockApiClient = type("RoborockApiClient", (), {})
    sys.modules.update(modules)


def _load_helper():
    _install_roborock_stubs()
    path = Path(__file__).with_name("roborock_cloud.py")
    spec = importlib.util.spec_from_file_location("roborock_cloud_test_subject", path)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class Q10CommandPayloadTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helper = _load_helper()

    def test_start_clean_uses_required_cmd_payload(self):
        dp, payload = self.helper.q10_command_payload("app_start", [])

        self.assertEqual(dp, _FakeB01Q10DP.START_CLEAN)
        self.assertEqual(payload, {"cmd": 1})

    def test_charge_uses_dock_task_command(self):
        dp, payload = self.helper.q10_command_payload("app_charge", None)

        self.assertEqual(dp, _FakeB01Q10DP.START_DOCK_TASK)
        self.assertEqual(payload, {})

    def test_custom_params_are_preserved(self):
        dp, payload = self.helper.q10_command_payload("app_start", {"cmd": 4})

        self.assertEqual(dp, _FakeB01Q10DP.START_CLEAN)
        self.assertEqual(payload, {"cmd": 4})


if __name__ == "__main__":
    unittest.main()
