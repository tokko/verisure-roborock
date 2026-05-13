#!/usr/bin/env python3
import argparse
import asyncio
import json
import os
import sys
from pathlib import Path
from typing import Any

from roborock.data.b01_q10.b01_q10_code_mappings import B01_Q10_DP
from roborock.data.containers import UserData
from roborock.devices.device_manager import UserParams, create_device_manager
from roborock.web_api import RoborockApiClient


def write(payload: dict[str, Any]) -> None:
    print(json.dumps(payload, separators=(",", ":")))


def read_stdin_json() -> dict[str, Any]:
    try:
        return json.loads(sys.stdin.read() or "{}")
    except json.JSONDecodeError as exc:
        raise SystemExit(f"invalid json request: {exc}") from exc


def load_user_data(path: str) -> UserData:
    try:
        with open(path, "r", encoding="utf-8") as f:
            return UserData.from_dict(json.load(f))
    except FileNotFoundError as exc:
        raise RuntimeError(f"Roborock auth cache not found at {path}; run login-code or login-password once") from exc


def pending_path(auth_path: str) -> str:
    return auth_path + ".pending.json"


def load_pending_login(api: RoborockApiClient, auth_path: str) -> None:
    try:
        with open(pending_path(auth_path), "r", encoding="utf-8") as f:
            pending = json.load(f)
    except FileNotFoundError:
        return
    device_identifier = pending.get("device_identifier")
    if device_identifier:
        api._device_identifier = device_identifier


def save_pending_login(api: RoborockApiClient, auth_path: str) -> None:
    p = Path(pending_path(auth_path))
    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.with_suffix(p.suffix + ".tmp")
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump({"device_identifier": api._device_identifier}, f, separators=(",", ":"))
    os.chmod(tmp, 0o600)
    os.replace(tmp, p)


def clear_pending_login(auth_path: str) -> None:
    try:
        os.remove(pending_path(auth_path))
    except FileNotFoundError:
        pass


def save_user_data(path: str, user_data: UserData) -> None:
    p = Path(path)
    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.with_suffix(p.suffix + ".tmp")
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(user_data.as_dict(), f, separators=(",", ":"))
    os.chmod(tmp, 0o600)
    os.replace(tmp, p)


async def login_code(req: dict[str, Any]) -> None:
    email = req["email"]
    auth_path = req["auth_path"]
    code = req["code"]
    api = RoborockApiClient(username=email)
    load_pending_login(api, auth_path)
    user_data = await api.code_login(code)
    save_user_data(auth_path, user_data)
    clear_pending_login(auth_path)
    write({"ok": True})


async def login_password(req: dict[str, Any]) -> None:
    email = req["email"]
    auth_path = req["auth_path"]
    password = req["password"]
    api = RoborockApiClient(username=email)
    user_data = await api.pass_login(password)
    save_user_data(auth_path, user_data)
    write({"ok": True})


async def request_code(req: dict[str, Any]) -> None:
    auth_path = req.get("auth_path")
    api = RoborockApiClient(username=req["email"])
    await api.request_code()
    if auth_path:
        save_pending_login(api, auth_path)
    write({"ok": True})


async def with_devices(req: dict[str, Any]):
    user_data = load_user_data(req["auth_path"])
    params = UserParams(username=req["email"], user_data=user_data)
    manager = await create_device_manager(params)
    try:
        return manager, await manager.get_devices()
    except Exception:
        await manager.close()
        raise


def device_summary(device: Any) -> dict[str, Any]:
    product = getattr(device, "product", None)
    return {
        "duid": getattr(device, "duid", None),
        "name": getattr(device, "name", None),
        "product": getattr(product, "model", None) or str(product or ""),
    }


def select_device(devices: list[Any], selector: str) -> Any:
    selector_folded = selector.casefold()
    for device in devices:
        candidates = [
            getattr(device, "duid", ""),
            getattr(device, "name", ""),
        ]
        if any(str(candidate).casefold() == selector_folded for candidate in candidates if candidate):
            return device
    known = ", ".join(filter(None, (getattr(device, "name", "") for device in devices)))
    raise RuntimeError(f"no Roborock app device matched {selector!r}; known devices: {known}")


def q10_state_id(status: dict[str, Any]) -> int:
    fault = status.get("fault") or 0
    if fault:
        return 12
    state = str(status.get("status") or "").casefold()
    task = str(status.get("cleanTaskType") or "").casefold()
    back_type = str(status.get("backType") or "").casefold()
    if "clean" in state or "clean" in task:
        return 5
    if "pause" in state or "pause" in task:
        return 10
    if "charge" in state:
        return 8
    if "back" in state or "back" in back_type or "return" in state:
        return 6
    if "sleep" in state:
        return 2
    return 3


async def q10_status(props: Any) -> list[dict[str, Any]]:
    await props.refresh()
    await asyncio.sleep(2)
    status = props.status.as_dict() if props.status else {}
    return [{
        "state": q10_state_id(status),
        "battery": status.get("battery", 0) or 0,
        "clean_time": status.get("cleanTime", 0) or 0,
        "clean_area": status.get("cleanArea", 0) or 0,
        "error_code": status.get("fault", 0) or 0,
    }]


async def q10_summary(props: Any) -> list[Any]:
    await props.refresh()
    await asyncio.sleep(2)
    status = props.status.as_dict() if props.status else {}
    return [
        status.get("totalCleanTime", 0) or 0,
        status.get("totalCleanArea", 0) or 0,
        status.get("totalCleanCount", 0) or 0,
        [],
    ]


async def q10_command(props: Any, command: str, params: Any) -> Any:
    mapped = q10_command_payload(command, params)
    if not mapped:
        raise RuntimeError(f"B01/Q10 command {command!r} is not supported")
    dp, payload = mapped
    return await props.command.send(dp, params=payload)


def q10_command_payload(command: str, params: Any) -> tuple[Any, Any] | None:
    if command == "app_start":
        return B01_Q10_DP.START_CLEAN, params or {"cmd": 1}
    if command == "app_resume":
        return B01_Q10_DP.RESUME, params or {}
    if command == "app_pause":
        return B01_Q10_DP.PAUSE, params or {}
    if command == "app_charge":
        return B01_Q10_DP.START_DOCK_TASK, params or {}
    if command == "app_stop":
        return B01_Q10_DP.STOP, params or {}
    return None


async def run_command(device: Any, command: str, params: Any) -> Any:
    if props := getattr(device, "v1_properties", None):
        if not getattr(props, "command", None):
            raise RuntimeError(f"device {getattr(device, 'name', '')!r} has invalid v1 command state")
        result = await props.command.send(command, params)
        if command == "get_clean_summary" and isinstance(result, dict):
            return [
                result.get("clean_time", 0) or 0,
                result.get("clean_area", 0) or 0,
                result.get("clean_count", 0) or 0,
                result.get("records", []) or [],
            ]
        return result

    if props := getattr(device, "b01_q10_properties", None):
        if command == "get_status":
            return await q10_status(props)
        if command == "get_clean_summary":
            return await q10_summary(props)
        return await q10_command(props, command, params)

    raise RuntimeError(f"device {getattr(device, 'name', '')!r} does not support known vacuum commands")


async def list_devices(req: dict[str, Any]) -> None:
    manager, devices = await with_devices(req)
    try:
        write({"devices": [device_summary(device) for device in devices]})
    finally:
        await manager.close()


async def rpc(req: dict[str, Any]) -> None:
    manager, devices = await with_devices(req)
    try:
        device = select_device(devices, req["selector"])
        result = await run_command(device, req["command"], req.get("params"))
        write({"result": result})
    finally:
        await manager.close()


async def main_async() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["request-code", "login-code", "login-password", "list", "rpc"])
    args = parser.parse_args()
    req = read_stdin_json()
    try:
        if args.action == "request-code":
            await request_code(req)
        elif args.action == "login-code":
            await login_code(req)
        elif args.action == "login-password":
            await login_password(req)
        elif args.action == "list":
            await list_devices(req)
        elif args.action == "rpc":
            await rpc(req)
    except Exception as exc:
        write({"error": str(exc)})
        return 1
    return 0


def main() -> int:
    return asyncio.run(main_async())


if __name__ == "__main__":
    raise SystemExit(main())
