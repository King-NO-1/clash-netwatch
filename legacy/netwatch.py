#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Clash NetWatch —— 让 Clash Verge 跟着当前网络自动开关「系统代理」与「虚拟网卡(TUN)」。

设计要点
--------
1. 网络探测：netsh 取无线 SSID 为主，Get-NetConnectionProfile 作为兜底（USB 共享等无 SSID 场景）。
2. 决策：SSID 命中 on_networks -> 开；未连接 / 命中 off_networks / 其它 -> 关。
3. 执行：
   - TUN 走 verge-mihomo 的控制器命名管道，PATCH /configs 热改 tun.enable（无需重启内核）。
   - 系统代理直接写 HKCU 注册表并刷新 WinINET（等效于 Verge 的 sysproxy 行为）。
   - 同步 verge.yaml，保证 Verge 重启后不会把状态翻回去。
4. 异常：探测失败不动作、切网冷却、漂移自愈、内核掉线宽限、休眠唤醒复检、单实例互斥。
"""

from __future__ import annotations

import argparse
import ctypes
import json
import os
import re
import subprocess
import sys
import threading
import time
import winreg
from pathlib import Path

APP_DIR = Path(__file__).resolve().parent
# NETWATCH_CONFIG 仅用于测试：指向一份临时配置，不动生产配置
CONFIG_PATH = Path(os.environ.get("NETWATCH_CONFIG") or (APP_DIR / "config.json"))
STATE_PATH = APP_DIR / "state.json"
PAUSE_FILE = APP_DIR / "paused.flag"
LOG_DIR = APP_DIR / "logs"
# NETWATCH_LOG 仅用于测试：把测试实例的日志和生产日志分开
LOG_PATH = Path(os.environ.get("NETWATCH_LOG") or (LOG_DIR / "netwatch.log"))

APP_NAME = "ClashNetWatch"
MUTEX_NAME = f"Local\\{APP_NAME}"
TASK_NAME = APP_NAME
UNSET = object()  # "尚未判定"哨兵，与 None（未连接）区分开

REG_PATH = r"Software\Microsoft\Windows\CurrentVersion\Internet Settings"
TUN_ADAPTER_NAMES = {"Meta", "Mihomo", "Clash", "utun", "TUN"}

DEFAULTS = {
    # 命中这些网络 -> 开代理 + TUN
    "on_networks": ["MyPhone", "MyPhonemagic6 pro2"],
    # 命中这些网络 -> 关代理 + TUN
    "off_networks": ["Campus-WiFi"],
    # 既不在开名单也不在关名单时（含未连接）的策略
    "default_action": "off",
    # 前缀容错：允许 "myphone" 匹配 "myphone-5g"
    "fuzzy_prefix_match": True,
    "poll_seconds": 3,
    # 连续多少次探测一致才认定网络已稳定
    "confirm_polls": 2,
    # 网络变化后等待 DHCP/路由就绪的时间
    "switch_cooldown_seconds": 8,
    # 漂移自检周期与最小重试间隔
    "drift_check_seconds": 30,
    "min_reapply_seconds": 45,
    # 内核（verge-mihomo）失联多久后先把系统代理摘掉，避免指向死端口
    "core_lost_grace_seconds": 20,
    # 同步 verge.yaml 里的 enable_tun_mode / enable_system_proxy
    "sync_verge_config": True,
    # 系统代理端口，null 表示从 verge.yaml 的 verge_mixed_port 读取
    "proxy_port": None,
    "core_wait_seconds": 180,
    "log_max_bytes": 1048576,
}


# ---------------------------------------------------------------- 日志
def _now() -> str:
    return time.strftime("%Y-%m-%d %H:%M:%S")


class Log:
    def __init__(self, path: Path, max_bytes: int = 1 << 20, console: bool = True):
        self.path = path
        self.max_bytes = max_bytes
        self.console = console
        path.parent.mkdir(parents=True, exist_ok=True)

    def _rotate(self):
        try:
            if self.path.exists() and self.path.stat().st_size > self.max_bytes:
                for i in (2, 1):
                    old = self.path.with_suffix(f".log.{i}")
                    if old.exists():
                        old.replace(self.path.with_suffix(f".log.{i + 1}"))
                self.path.replace(self.path.with_suffix(".log.1"))
        except OSError:
            pass

    def __call__(self, level: str, msg: str):
        line = f"[{_now()}] {level:<5} {msg}"
        if self.console:
            try:
                print(line, flush=True)
            except Exception:
                pass
        try:
            self._rotate()
            with self.path.open("a", encoding="utf-8") as f:
                f.write(line + "\n")
        except OSError:
            pass

    def info(self, msg: str):
        self("INFO", msg)

    def warn(self, msg: str):
        self("WARN", msg)

    def error(self, msg: str):
        self("ERROR", msg)

    def debug(self, msg: str):
        if os.environ.get("NETWATCH_DEBUG"):
            self("DEBUG", msg)


LOG = Log(LOG_PATH)


# ---------------------------------------------------------------- 配置
def load_config() -> dict:
    cfg = dict(DEFAULTS)
    if CONFIG_PATH.exists():
        try:
            user = json.loads(CONFIG_PATH.read_text(encoding="utf-8-sig"))
            if not isinstance(user, dict):
                raise ValueError("config.json 顶层必须是对象")
            unknown = set(user) - set(DEFAULTS)
            for k, v in user.items():
                cfg[k] = v
            if unknown:
                LOG.warn(f"config.json 中有未知字段（已忽略）：{sorted(unknown)}")
        except Exception as exc:  # noqa: BLE001
            LOG.error(f"config.json 解析失败，回退默认配置：{exc}")
    else:
        try:
            CONFIG_PATH.write_text(
                json.dumps(DEFAULTS, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
            )
            LOG.info(f"未找到 config.json，已写出默认配置：{CONFIG_PATH}")
        except OSError as exc:
            LOG.warn(f"写出默认配置失败：{exc}")
    return cfg


# ---------------------------------------------------------------- Clash Verge 侧信息
def verge_dirs() -> list[Path]:
    roots = []
    appdata = os.environ.get("APPDATA")
    if appdata:
        roots.append(Path(appdata) / "io.github.clash-verge-rev.clash-verge-rev")
        roots.append(Path(appdata) / "clash-verge")
    local = os.environ.get("LOCALAPPDATA")
    if local:
        roots.append(Path(local) / "io.github.clash-verge-rev.clash-verge-rev")
    return [p for p in roots if p.is_dir()]


def find_verge_dir() -> Path | None:
    for d in verge_dirs():
        if (d / "verge.yaml").exists():
            return d
    return None


def read_verge_yaml(verge_dir: Path | None) -> dict:
    """只解析少数几个扁平键，够用且不怕 YAML 结构复杂。"""
    out: dict = {}
    if not verge_dir:
        return out
    try:
        text = (verge_dir / "verge.yaml").read_text(encoding="utf-8", errors="replace")
    except OSError:
        return out
    for key in ("enable_tun_mode", "enable_system_proxy", "verge_mixed_port", "clash_core"):
        m = re.search(rf"(?m)^{key}:[ \t]*(\S+)[ \t]*$", text)
        if m:
            raw = m.group(1)
            if raw.lower() in ("true", "false"):
                out[key] = raw.lower() == "true"
            elif raw.isdigit():
                out[key] = int(raw)
            else:
                out[key] = raw.strip("'\"")
    return out


def sync_verge_yaml(verge_dir: Path | None, tun: bool, sysproxy: bool) -> bool:
    """把状态写回 verge.yaml，避免 Verge 重启/内核重载时回滚。"""
    if not verge_dir:
        return False
    path = verge_dir / "verge.yaml"
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        LOG.warn(f"读取 verge.yaml 失败：{exc}")
        return False
    new = text
    for key, val in (("enable_tun_mode", tun), ("enable_system_proxy", sysproxy)):
        new = re.sub(rf"(?m)^{key}:.*$", f"{key}: {'true' if val else 'false'}", new, count=1)
    if new == text:
        return False
    try:
        tmp = path.with_suffix(".yaml.netwatch.tmp")
        tmp.write_text(new, encoding="utf-8")
        os.replace(tmp, path)
        LOG.info(f"已同步 verge.yaml：enable_tun_mode={tun} enable_system_proxy={sysproxy}")
        return True
    except OSError as exc:
        LOG.warn(f"写入 verge.yaml 失败（Verge 可能正占用）：{exc}")
        return False


# ---------------------------------------------------------------- 内核控制器（命名管道 HTTP）
def _pipe_names() -> list[str]:
    try:
        return os.listdir("\\\\.\\pipe\\")
    except OSError:
        return []


def find_core_pipes() -> list[str]:
    names = [n for n in _pipe_names() if n.startswith("verge-mihomo")]
    # production 优先，其余按名字排序保证稳定
    names.sort(key=lambda n: (0 if "production" in n else 1, n))
    return [f"\\\\.\\pipe\\{n}" for n in names]


_SECRET_CACHE: dict = {"secret": None}


def _secret_candidates(verge_dir: Path | None) -> list[str]:
    cands: list[str] = []
    if _SECRET_CACHE["secret"] is not None:
        cands.append(_SECRET_CACHE["secret"])
    if verge_dir:
        for fname in ("config.yaml", "clash-verge.yaml"):
            try:
                m = re.search(
                    r"(?m)^secret:[ \t]*(\S+)[ \t]*$",
                    (verge_dir / fname).read_text(encoding="utf-8", errors="replace"),
                )
            except OSError:
                continue
            if m and m.group(1) not in cands:
                cands.append(m.group(1))
    for extra in ("set-your-secret", ""):
        if extra not in cands:
            cands.append(extra)
    return cands


def _read_http(sock) -> tuple[int, bytes]:
    buf = b""
    while b"\r\n\r\n" not in buf:
        chunk = sock.read(4096)
        if not chunk:
            break
        buf += chunk
    head, _, rest = buf.partition(b"\r\n\r\n")
    lines = head.split(b"\r\n")
    if not lines or len(lines[0].split()) < 2:
        raise OSError("控制器返回了非 HTTP 响应")
    status = int(lines[0].split()[1])
    headers = {}
    for line in lines[1:]:
        k, _, v = line.partition(b":")
        headers[k.strip().lower().decode("latin-1")] = v.strip().decode("latin-1")
    length = int(headers.get("content-length", "0") or 0)
    body = rest
    while len(body) < length:
        chunk = sock.read(4096)
        if not chunk:
            break
        body += chunk
    return status, body[:length] if length else body


def core_api(method: str, path: str, body: dict | None = None, timeout: float = 5.0):
    """对 verge-mihomo 控制器发一条请求，返回 (status, dict|None)。管道不存在/超时返回 (None, None)。

    管道读写在子线程里执行并带超时——内核卡死时不能把守护进程一起挂住。
    """
    if _pipe_cooldown_active():
        return None, None
    verge_dir = find_verge_dir()
    payload = json.dumps(body, ensure_ascii=False).encode() if body is not None else None
    last_exc: Exception | None = None
    for pipe in find_core_pipes():
        for secret in _secret_candidates(verge_dir):
            req = [f"{method} {path} HTTP/1.1", "Host: localhost", "Connection: close"]
            if secret:
                req.append(f"Authorization: Bearer {secret}")
            if payload is not None:
                req.append("Content-Type: application/json")
                req.append(f"Content-Length: {len(payload)}")
            raw = ("\r\n".join(req) + "\r\n\r\n").encode() + (payload or b"")

            def _attempt(pipe: str = pipe, raw: bytes = raw):
                with open(pipe, "r+b", buffering=0) as f:
                    f.write(raw)
                    return _read_http(f)

            try:
                status, resp = _call_with_timeout(_attempt, timeout)
            except (OSError, ValueError) as exc:
                last_exc = exc
                break  # 换下一个管道
            if status is None:  # 超时
                _pipe_note_timeout()
                return None, None
            if status == 401:
                continue  # 换下一个密钥
            _pipe_note_ok()
            _SECRET_CACHE["secret"] = secret
            if not resp:
                return status, None
            try:
                return status, json.loads(resp.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                return status, {"_raw": resp.decode("utf-8", "replace")}
    if last_exc is not None:
        LOG.debug(f"控制器不可用：{last_exc}")
    return None, None


_PIPE_STATE = {"timeouts": 0, "cooldown_until": 0.0}


def _pipe_cooldown_active() -> bool:
    return time.time() < _PIPE_STATE["cooldown_until"]


def _pipe_note_ok():
    _PIPE_STATE["timeouts"] = 0


def _pipe_note_timeout():
    _PIPE_STATE["timeouts"] += 1
    if _PIPE_STATE["timeouts"] >= 2:
        _PIPE_STATE["cooldown_until"] = time.time() + 60
        _PIPE_STATE["timeouts"] = 0
        LOG.error("内核控制器连续超时，冷却 60 秒后再试（避免线程堆积）")


def _call_with_timeout(fn, timeout: float):
    """在守护线程里跑阻塞调用；超时返回 (None, None)，线程被放弃而不是等它。"""
    box: list = [None, None]

    def runner():
        try:
            box[0], box[1] = fn()
        except BaseException as exc:  # noqa: BLE001
            box[1] = exc

    th = threading.Thread(target=runner, daemon=True, name="core-api")
    th.start()
    th.join(timeout)
    if th.is_alive():
        LOG.warn(f"内核控制器 {timeout:.0f}s 无响应，放弃本次调用")
        return None, None
    if isinstance(box[1], BaseException):
        raise box[1]
    return box[0], box[1]


def core_version() -> str | None:
    status, data = core_api("GET", "/version")
    if status == 200 and isinstance(data, dict):
        return str(data.get("version"))
    return None


def get_tun_config() -> dict | None:
    status, data = core_api("GET", "/configs")
    if status == 200 and isinstance(data, dict):
        tun = data.get("tun")
        return tun if isinstance(tun, dict) else None
    return None


def set_tun(enable: bool) -> tuple[bool, str]:
    tun = get_tun_config()
    if tun is None:
        return False, "读不到内核配置（/configs 无响应）"
    if bool(tun.get("enable")) == enable:
        return True, "已是目标值"
    target = dict(tun)
    target["enable"] = enable
    status, _ = core_api("PATCH", "/configs", {"tun": target})
    if status not in (200, 204):
        return False, f"PATCH /configs 返回 {status}"
    # 回读确认（TUN 建/拆有几百毫秒延迟），整体不超过 6 秒
    deadline = time.time() + 6
    while time.time() < deadline:
        time.sleep(0.5)
        now = get_tun_config()
        if now is not None and bool(now.get("enable")) == enable:
            return True, "已生效"
    return False, "PATCH 成功但回读未确认"


# ---------------------------------------------------------------- 系统代理（注册表）
def _notify_wininet():
    try:
        wininet = ctypes.windll.Wininet
        wininet.InternetSetOptionW(None, 39, None, 0)  # SETTINGS_CHANGED
        wininet.InternetSetOptionW(None, 37, None, 0)  # REFRESH
    except Exception as exc:  # noqa: BLE001
        LOG.debug(f"刷新 WinINET 失败：{exc}")


def read_sysproxy() -> dict:
    out = {"enable": False, "server": None}
    try:
        with winreg.OpenKey(winreg.HKEY_CURRENT_USER, REG_PATH) as key:
            try:
                out["enable"] = bool(winreg.QueryValueEx(key, "ProxyEnable")[0])
            except FileNotFoundError:
                pass
            try:
                out["server"] = str(winreg.QueryValueEx(key, "ProxyServer")[0])
            except FileNotFoundError:
                pass
    except OSError:
        pass
    return out


def set_sysproxy(enable: bool, port: int) -> tuple[bool, str]:
    server = f"127.0.0.1:{port}"
    cur = read_sysproxy()
    if enable and cur["enable"] and cur["server"] == server:
        return True, "已是目标值"
    if not enable and not cur["enable"]:
        return True, "已是关闭"
    try:
        with winreg.CreateKeyEx(winreg.HKEY_CURRENT_USER, REG_PATH, 0, winreg.KEY_READ | winreg.KEY_WRITE) as key:
            if enable:
                winreg.SetValueEx(key, "ProxyServer", 0, winreg.REG_SZ, server)
                winreg.SetValueEx(key, "ProxyEnable", 0, winreg.REG_DWORD, 1)
            else:
                winreg.SetValueEx(key, "ProxyEnable", 0, winreg.REG_DWORD, 0)
    except OSError as exc:
        return False, f"写注册表失败：{exc}"
    _notify_wininet()
    cur = read_sysproxy()
    if cur["enable"] == enable and (not enable or cur["server"] == server):
        return True, "已生效"
    return False, f"写后回读不一致（enable={cur['enable']} server={cur['server']}）"


# ---------------------------------------------------------------- 网络探测
SSID_RE = re.compile(r"(?m)^[ \t]+SSID[ \t]*:[ \t]*(.+?)[ \t]*$")
IFACE_RE = re.compile(r"(?m)^[ \t]*(?:名称|Name)[ \t]*:[ \t]*(.+?)[ \t]*$")


def _decode(raw: bytes) -> str:
    for enc in ("utf-8", "gbk", "cp936"):
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            continue
    return raw.decode("utf-8", "replace")


def _run(cmd: list[str], timeout: float = 10.0):
    flags = getattr(subprocess, "CREATE_NO_WINDOW", 0)
    try:
        p = subprocess.run(cmd, capture_output=True, timeout=timeout, creationflags=flags)
        return p.returncode, p.stdout
    except (OSError, subprocess.TimeoutExpired) as exc:
        LOG.debug(f"命令失败 {cmd[0]}: {exc}")
        return -1, b""


def probe_wlan() -> dict:
    """返回 {state: connected|disconnected|unknown, ssid, iface, source, note}"""
    rc, out = _run(["netsh", "wlan", "show", "interfaces"])
    if rc != 0:
        return {"state": "unknown", "ssid": None, "iface": None, "source": "netsh", "note": f"netsh 退出码 {rc}"}
    text = _decode(out)
    m = SSID_RE.search(text)
    if m:
        iface = IFACE_RE.search(text)
        return {
            "state": "connected",
            "ssid": m.group(1).strip(),
            "iface": iface.group(1).strip() if iface else None,
            "source": "netsh",
            "note": "",
        }
    if re.search(r"已断开|未连接|disconnected", text, re.I):
        return {"state": "disconnected", "ssid": None, "iface": None, "source": "netsh", "note": "无线已断开"}
    return {"state": "unknown", "ssid": None, "iface": None, "source": "netsh", "note": "输出中没有 SSID 行"}


_PROFILE_CACHE: dict = {"ts": 0.0, "obs": None}


def probe_profiles() -> dict:
    """兜底探测。PowerShell 启动有成本，缓存 6 秒——只用于 WiFi 拿不到 SSID 的场景。"""
    now = time.time()
    cached = _PROFILE_CACHE["obs"]
    if cached is not None and now - _PROFILE_CACHE["ts"] < 6:
        return dict(cached)
    obs = _probe_profiles_now()
    _PROFILE_CACHE.update(ts=now, obs=obs)
    return dict(obs)


def _probe_profiles_now() -> dict:
    ps = (
        "[Console]::OutputEncoding=[Text.Encoding]::UTF8;"
        "Get-NetConnectionProfile | Select-Object InterfaceAlias,Name | ConvertTo-Json -Compress"
    )
    rc, out = _run(["powershell", "-NoProfile", "-NonInteractive", "-Command", ps], timeout=15)
    if rc != 0 or not out.strip():
        return {"state": "unknown", "ssid": None, "iface": None, "source": "profile", "note": "查询连接配置文件失败"}
    try:
        data = json.loads(_decode(out))
    except json.JSONDecodeError:
        return {"state": "unknown", "ssid": None, "iface": None, "source": "profile", "note": "返回非法 JSON"}
    if isinstance(data, dict):
        data = [data]
    for item in data:
        alias = str(item.get("InterfaceAlias") or "")
        name = str(item.get("Name") or "").strip()
        if not name or alias in TUN_ADAPTER_NAMES:
            continue
        if re.match(r"^(Meta|Mihomo|Clash)", alias, re.I):
            continue
        return {"state": "connected", "ssid": name, "iface": alias, "source": "profile", "note": "无 SSID，使用配置名"}
    return {"state": "disconnected", "ssid": None, "iface": None, "source": "profile", "note": "没有活动连接"}


def probe_network() -> dict:
    obs = probe_wlan()
    if obs["state"] == "connected" and obs["ssid"]:
        return obs
    fb = probe_profiles()
    if obs["state"] == "disconnected" and fb["state"] == "disconnected":
        return obs
    if fb["state"] == "connected":
        fb["note"] = f"{obs['note']}；已用连接配置文件兜底" if obs["note"] else "连接配置文件兜底"
        return fb
    return obs if obs["state"] != "unknown" else (fb if fb["state"] != "unknown" else obs)


# ---------------------------------------------------------------- 决策
def _norm(s: str) -> str:
    return re.sub(r"[\s_\-·.]+", "", s or "").strip().lower()


def match_network(ssid: str | None, patterns: list[str], fuzzy: bool) -> str | None:
    if not ssid:
        return None
    ns = _norm(ssid)
    for pat in patterns or []:
        np_ = _norm(str(pat))
        if len(np_) < 2:
            continue
        if ns == np_:
            return pat
        if fuzzy and len(np_) >= 4 and (ns.startswith(np_) or np_.startswith(ns)):
            return pat
    return None


def decide(obs: dict, cfg: dict) -> tuple[str, str]:
    if obs["state"] != "connected" or not obs["ssid"]:
        if obs["state"] == "unknown":
            return "unknown", f"探测失败：{obs.get('note') or '未知原因'}"
        return "off", "当前未连接任何网络（断开即关闭）"
    hit_on = match_network(obs["ssid"], cfg["on_networks"], bool(cfg["fuzzy_prefix_match"]))
    if hit_on:
        return "on", f"SSID「{obs['ssid']}」命中放行名单（规则 {hit_on!r}）"
    hit_off = match_network(obs["ssid"], cfg["off_networks"], bool(cfg["fuzzy_prefix_match"]))
    if hit_off:
        return "off", f"SSID「{obs['ssid']}」命中关闭名单（规则 {hit_off!r}）"
    return str(cfg["default_action"]), f"SSID「{obs['ssid']}」不在任何名单，按默认策略 {cfg['default_action']}"


# ---------------------------------------------------------------- 执行
def proxy_port(cfg: dict, verge_dir: Path | None) -> int:
    if cfg.get("proxy_port"):
        try:
            port = int(cfg["proxy_port"])
            if 0 < port < 65536:
                return port
        except (TypeError, ValueError):
            LOG.warn(f"config.json 的 proxy_port 非法（{cfg['proxy_port']!r}），改用 verge.yaml")
    v = read_verge_yaml(verge_dir).get("verge_mixed_port")
    if isinstance(v, int) and 0 < v < 65536:
        return v
    return 7897


def runtime_state(cfg: dict) -> dict:
    tun = get_tun_config()
    sp = read_sysproxy()
    port = proxy_port(cfg, find_verge_dir())
    return {
        "core": tun is not None,
        "tun": bool(tun.get("enable")) if tun else None,
        "sysproxy": sp["enable"],
        "sysproxy_server": sp["server"],
        "proxy_port": port,
        "sysproxy_ok": sp["enable"] and sp["server"] == f"127.0.0.1:{port}",
    }


def apply_target(target: str, cfg: dict, dry: bool = False, reason: str = "") -> dict:
    """把运行态推到 target（on/off）。返回执行结果。"""
    want = target == "on"
    verge_dir = find_verge_dir()
    port = proxy_port(cfg, verge_dir)
    result = {"target": target, "changed": False, "steps": [], "ok": True}

    if dry:
        LOG.info(f"[dry-run] 目标={target}（{reason}）端口={port}，不写入任何状态")
        result["steps"].append("dry-run")
        return result

    cur = runtime_state(cfg)
    LOG.info(f"执行 {target.upper()}（{reason}）当前 tun={cur['tun']} sysproxy={cur['sysproxy']} core={cur['core']}")

    if not cur["core"]:
        if want:
            result["ok"] = False
            result["steps"].append("内核未运行，跳过（等下轮重试）")
            LOG.warn("内核（verge-mihomo）不可达，暂不开启代理，保持现状等待重试")
            return result
        # 关的时候不依赖内核，但 TUN 关不掉就要报出来
    # 顺序：开 -> 先 TUN 后系统代理；关 -> 先系统代理后 TUN
    order = [("tun", want), ("sysproxy", want)] if want else [("sysproxy", False), ("tun", False)]

    for kind, val in order:
        if kind == "sysproxy":
            if cur["sysproxy"] == val and (not val or cur["sysproxy_server"] == f"127.0.0.1:{port}"):
                result["steps"].append("sysproxy 已是目标值")
                continue
            ok, msg = set_sysproxy(val, port)
        else:
            if cur["tun"] is None:
                ok, msg = False, "内核不可达"
            else:
                ok, msg = set_tun(val)
        result["steps"].append(f"{kind}={'on' if val else 'off'} {msg}")
        LOG.info(f"  {kind} -> {'on' if val else 'off'}：{msg}")
        if ok and "已是" not in msg:
            result["changed"] = True
        if not ok:
            result["ok"] = False
            if want:
                # 开不起来就把系统代理一起摘掉，避免指向一个没有内核的端口
                if kind == "tun":
                    set_sysproxy(False, port)
                    LOG.warn("TUN 开启失败，已回退关闭系统代理")
                break

    if cfg.get("sync_verge_config"):
        if result["ok"]:
            sync_verge_yaml(verge_dir, want, want)
        else:
            # 部分成功时按实际运行态同步，别让 Verge 重启后翻回一个更糟的状态
            now = runtime_state(cfg)
            sync_verge_yaml(verge_dir, bool(now["tun"]), bool(now["sysproxy"]))
    return result


# ---------------------------------------------------------------- 状态文件 / 单实例
def write_state(data: dict):
    try:
        STATE_PATH.write_text(
            json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
    except OSError:
        pass


def single_instance() -> bool:
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    handle = kernel32.CreateMutexW(None, False, MUTEX_NAME)
    err = ctypes.get_last_error()  # 必须紧跟 CreateMutexW 读取，中间不能有别的调用
    if not handle:
        LOG.warn(f"创建互斥体失败（错误码 {err}），跳过单实例检查")
        return True
    if err == 183:  # ERROR_ALREADY_EXISTS
        kernel32.CloseHandle(handle)
        return False
    single_instance._handle = handle  # 保持引用，进程退出时自动释放
    return True


# ---------------------------------------------------------------- 主循环
def run_daemon(cfg: dict, dry: bool, simulate: str | None):
    if not single_instance():
        LOG.info("已有实例在运行，本进程退出")
        return 0
    LOG.info("=" * 60)
    LOG.info(f"NetWatch 启动 | poll={cfg['poll_seconds']}s confirm={cfg['confirm_polls']} dry_run={dry}")
    LOG.info(f"放行名单={cfg['on_networks']} 关闭名单={cfg['off_networks']} 默认={cfg['default_action']}")

    # 等内核就绪（Verge 可能还在启动）
    deadline = time.time() + int(cfg["core_wait_seconds"])
    next_nag = 0.0
    while time.time() < deadline:
        ver = core_version()
        if ver:
            LOG.info(f"内核就绪：{ver}")
            break
        if time.time() >= next_nag:
            LOG.info("等待 Clash Verge 内核…")
            next_nag = time.time() + 10
        time.sleep(3)
    else:
        LOG.warn("等待内核超时，继续运行（联网状态变化时会重试）")

    last_pending_ssid = object()
    pending_count = 0
    # 用哨兵而不是 None：None 是合法的观测值（未连接），拿它当"尚未判定"会把
    # "唤醒后发现已断网"这种场景永远判不出来
    stable_ssid: object = UNSET
    target: str | None = None      # 最近一次判定出的目标状态，漂移自愈以它为准
    in_sync = False                # 运行态是否已确认等于 target
    last_apply_ts = 0.0
    last_tick = time.time()
    last_drift_check = 0.0
    drift_hits = 0
    core_lost_since: float | None = None
    paused_logged = False
    fail_streak = 0

    while True:
        tick_start = time.time()
        gap = tick_start - last_tick
        last_tick = tick_start
        if gap > max(cfg["poll_seconds"] * 3, 30) + 10:
            LOG.info(f"检测到时钟跳变 {gap:.0f}s（休眠/待机唤醒），立即复检")
            pending_count = 0
            last_pending_ssid = object()
            stable_ssid = UNSET  # 强制重新判定

        if PAUSE_FILE.exists():
            if not paused_logged:
                LOG.info(f"发现 {PAUSE_FILE.name}，暂停自动切换")
                paused_logged = True
            time.sleep(cfg["poll_seconds"])
            continue
        paused_logged = False

        obs = probe_network()
        if simulate:
            if simulate == "__none__":
                obs = {"state": "disconnected", "ssid": None, "iface": None,
                       "source": "simulate", "note": "模拟断网"}
            else:
                obs = {"state": "connected", "ssid": simulate, "iface": "SIM",
                       "source": "simulate", "note": ""}

        if obs["state"] == "unknown":
            fail_streak += 1
            LOG.warn(f"网络探测失败（{fail_streak} 次）：{obs['note']}；保持当前状态")
            time.sleep(cfg["poll_seconds"])
            continue
        fail_streak = 0

        # 去抖：连续 confirm_polls 次一致才认账
        if obs["ssid"] != last_pending_ssid:
            last_pending_ssid = obs["ssid"]
            pending_count = 1
        else:
            pending_count += 1

        if pending_count >= int(cfg["confirm_polls"]) and obs["ssid"] != stable_ssid:
            stable_ssid = obs["ssid"]
            desired, why = decide(obs, cfg)
            LOG.info(f"网络稳定：{stable_ssid or '<未连接>'} | {why} | 目标={desired.upper()}")
            if desired in ("on", "off"):
                target = desired
                # 切网冷却：等 DHCP/路由就绪
                if time.time() - last_apply_ts < cfg["switch_cooldown_seconds"]:
                    wait = cfg["switch_cooldown_seconds"] - (time.time() - last_apply_ts)
                    LOG.info(f"切网冷却中，等待 {wait:.1f}s 后再执行")
                    time.sleep(max(0.0, wait))
                res = apply_target(target, cfg, dry=dry, reason=why)
                in_sync = res["ok"]
                last_apply_ts = time.time()
                drift_hits = 0
                write_state({
                    "ts": _now(), "ssid": stable_ssid, "desired": target,
                    "applied": target if res["ok"] else None, "ok": res["ok"],
                    "steps": res["steps"], "pid": os.getpid(),
                })
            else:
                LOG.warn(f"探测不可判定（{why}），保持状态不变")

        # 漂移自检：以「判定目标」为准，被 Verge/用户改动就拉回来；未确认则重建
        if target is not None and time.time() - last_drift_check >= cfg["drift_check_seconds"]:
            last_drift_check = time.time()
            cur = runtime_state(cfg)
            if not cur["core"]:
                if core_lost_since is None:
                    core_lost_since = time.time()
                    LOG.warn("内核失联（可能处于轻量模式或正在重启），开始宽限计时")
                elif (
                    target == "on"
                    and time.time() - core_lost_since > cfg["core_lost_grace_seconds"]
                    and cur["sysproxy"]
                ):
                    LOG.warn("内核失联超过宽限期，先摘掉系统代理避免网络不可用")
                    set_sysproxy(False, cur["proxy_port"])
                    in_sync = False
            else:
                if core_lost_since is not None:
                    LOG.info("内核恢复，重新校验状态")
                    core_lost_since = None
                    last_apply_ts = 0.0
                    in_sync = False
                if not in_sync:
                    # 上次应用失败，或内核恢复后状态不明：按当前网络重建
                    if time.time() - last_apply_ts >= cfg["min_reapply_seconds"]:
                        LOG.info(f"状态重建：目标={target.upper()}")
                        res = apply_target(target, cfg, dry=dry, reason="状态重建")
                        in_sync = res["ok"]
                        last_apply_ts = time.time()
                        drift_hits = 0
                        write_state({
                            "ts": _now(), "ssid": stable_ssid, "desired": target,
                            "applied": target if res["ok"] else None, "ok": res["ok"],
                            "steps": res["steps"], "pid": os.getpid(),
                        })
                else:
                    want = target == "on"
                    bad = []
                    if want:
                        if cur["tun"] is not True:
                            bad.append(f"tun={cur['tun']}")
                        if not cur["sysproxy_ok"]:
                            bad.append(f"sysproxy={cur['sysproxy']}/{cur['sysproxy_server']}")
                    else:
                        if cur["tun"] is True:
                            bad.append("tun=True")
                        if cur["sysproxy"]:
                            bad.append("sysproxy=True")
                    if bad:
                        drift_hits += 1
                        LOG.warn(f"运行态漂移（{drift_hits}/2）：{', '.join(bad)}")
                        if drift_hits >= 2 and time.time() - last_apply_ts >= cfg["min_reapply_seconds"]:
                            drift_hits = 0
                            res = apply_target(target, cfg, dry=dry, reason="漂移自愈")
                            in_sync = res["ok"]
                            last_apply_ts = time.time()
                    else:
                        drift_hits = 0
                        # Verge 改设置时会整体重写 verge.yaml，可能把持久化状态覆盖掉
                        if cfg.get("sync_verge_config"):
                            vy = read_verge_yaml(find_verge_dir())
                            if (vy.get("enable_tun_mode") is not want) or (vy.get("enable_system_proxy") is not want):
                                LOG.info("verge.yaml 与运行态不一致，重新同步")
                                sync_verge_yaml(find_verge_dir(), want, want)

        sleep_for = cfg["poll_seconds"] - (time.time() - tick_start)
        if sleep_for < 0.2:
            sleep_for = 0.2
        # 分段睡眠，pause/退出更灵敏
        while sleep_for > 0:
            step = min(0.5, sleep_for)
            time.sleep(step)
            sleep_for -= step
            if PAUSE_FILE.exists():
                break


# ---------------------------------------------------------------- 子命令
def cmd_status(cfg: dict, as_json: bool):
    obs = probe_network()
    desired, why = decide(obs, cfg)
    cur = runtime_state(cfg)
    st = {}
    if STATE_PATH.exists():
        try:
            st = json.loads(STATE_PATH.read_text(encoding="utf-8"))
        except json.JSONDecodeError:
            st = {}
    want = desired == "on"
    data = {
        "time": _now(),
        "network": obs,
        "desired": desired,
        "reason": why,
        "runtime": cur,
        "last_change": st,
        "in_sync": None if desired == "unknown" else ((cur["tun"] is want) and (cur["sysproxy_ok"] is want)),
    }
    if as_json:
        print(json.dumps(data, ensure_ascii=False, indent=2))
        return 0
    print(f"时间       : {data['time']}")
    print(f"当前网络   : {obs['ssid'] or '<未连接>'}  (来源 {obs['source']}, 状态 {obs['state']})")
    print(f"判定       : {desired.upper()}  — {why}")
    print(f"内核       : {'在线 ' + (core_version() or '') if cur['core'] else '不可达'}")
    print(f"TUN        : {cur['tun']}")
    print(f"系统代理   : {cur['sysproxy']}  server={cur['sysproxy_server']} (期望端口 {cur['proxy_port']})")
    if data["in_sync"] is None:
        print("是否一致   : 未知（探测失败，守护进程会保持现状）")
    else:
        print(f"是否一致   : {'是' if data['in_sync'] else '否'}")
    if st:
        print(f"上次变更   : {st.get('ts')} -> {st.get('desired')} (ok={st.get('ok')}) {st.get('ssid')}")
    return 0


def cmd_doctor(cfg: dict):
    print("== NetWatch 体检 ==")
    print(f"脚本位置   : {APP_DIR}")
    print(f"配置文件   : {CONFIG_PATH} {'[OK]' if CONFIG_PATH.exists() else '[缺失，将用默认值]'}")
    verge = find_verge_dir()
    print(f"Verge 目录 : {verge or '[未找到]'}")
    if verge:
        vy = read_verge_yaml(verge)
        print(f"verge.yaml : enable_tun_mode={vy.get('enable_tun_mode')} "
              f"enable_system_proxy={vy.get('enable_system_proxy')} mixed_port={vy.get('verge_mixed_port')}")

    pipes = find_core_pipes()
    print(f"控制器管道 : {pipes if pipes else '[未发现，内核未运行]'}")
    print(f"内核版本   : {core_version() or '不可达'}")
    cur = runtime_state(cfg)
    print(f"运行态     : tun={cur['tun']} sysproxy={cur['sysproxy']}({cur['sysproxy_server']}) 端口={cur['proxy_port']}")

    obs = probe_network()
    print(f"网络探测   : state={obs['state']} ssid={obs['ssid']!r} iface={obs['iface']!r} source={obs['source']} {obs['note']}")
    desired, why = decide(obs, cfg)
    print(f"判定       : {desired} — {why}")

    print("-- SSID 匹配矩阵 --")
    for group in ("on_networks", "off_networks"):
        for pat in cfg[group]:
            hit = match_network(obs["ssid"], [pat], bool(cfg["fuzzy_prefix_match"]))
            print(f"  [{group}] {pat!r} -> {'命中' if hit else '不命中'}")
    print(f"  当前可见: {obs['ssid'] or '无'}")

    task = task_registered()
    print(f"计划任务   : {task}")
    if PAUSE_FILE.exists():
        print(f"暂停标志   : 存在（{PAUSE_FILE}）—— 自动切换已被暂停")
    if LOG_PATH.exists():
        lines = LOG_PATH.read_text(encoding="utf-8", errors="replace").strip().splitlines()[-5:]
        print("-- 最近日志 --")
        for ln in lines:
            print("  " + ln)
    return 0


def cmd_check(cfg: dict, simulate: str | None):
    if simulate:
        obs = {"state": "connected", "ssid": simulate, "iface": "SIM", "source": "simulate", "note": ""}
    else:
        obs = probe_network()
    desired, why = decide(obs, cfg)
    print(f"网络={obs['ssid'] or '<未连接>'} 目标={desired.upper()} 原因={why}")
    return 0


def cmd_selftest(cfg: dict):
    """端到端自检：关 -> 开 -> 再开（幂等）-> 复原。会真实改动系统代理与 TUN。"""
    print("== 自检开始（会真实开关一次代理）==")
    checks: list[tuple[str, bool, str]] = []
    original = runtime_state(cfg)

    def check(name: str, cond: bool, detail: str = ""):
        checks.append((name, cond, detail))
        print(f"  [{'PASS' if cond else 'FAIL'}] {name} {detail}")

    r = apply_target("off", cfg, reason="selftest:off")
    cur = runtime_state(cfg)
    check("关闭 TUN", cur["tun"] is False, f"tun={cur['tun']}")
    check("关闭系统代理", cur["sysproxy"] is False, f"sysproxy={cur['sysproxy']}")
    check("关闭步骤全部成功", r["ok"], str(r["steps"]))

    r = apply_target("on", cfg, reason="selftest:on")
    cur = runtime_state(cfg)
    check("开启 TUN", cur["tun"] is True, f"tun={cur['tun']}")
    check("开启系统代理", cur["sysproxy_ok"] is True, f"sysproxy={cur['sysproxy']}/{cur['sysproxy_server']}")
    check("开启步骤全部成功", r["ok"], str(r["steps"]))

    vy = read_verge_yaml(find_verge_dir())
    check("verge.yaml 已同步为开启", vy.get("enable_tun_mode") is True and vy.get("enable_system_proxy") is True, str(vy))

    r2 = apply_target("on", cfg, reason="selftest:idempotent")
    check("重复应用幂等（无变更）", all("已是" in s or "已是关闭" in s for s in r2["steps"]), str(r2["steps"]))

    # 复原到自检前的状态
    restore = "on" if (original["tun"] and original["sysproxy"]) else "off"
    apply_target(restore, cfg, reason="selftest:restore")
    cur = runtime_state(cfg)
    check("已复原到自检前状态", cur["tun"] == original["tun"] and cur["sysproxy"] == original["sysproxy"],
          f"tun={cur['tun']} sysproxy={cur['sysproxy']}")

    failed = [c for c in checks if not c[1]]
    print(f"== 自检结束：{len(checks) - len(failed)}/{len(checks)} 通过 ==")
    return 0 if not failed else 1


def cmd_install():
    ps1 = APP_DIR / "install-task.ps1"
    flags = getattr(subprocess, "CREATE_NO_WINDOW", 0)
    p = subprocess.run(
        ["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", str(ps1)],
        capture_output=True, creationflags=flags,
    )
    print(_decode(p.stdout))
    if p.returncode != 0:
        print(_decode(p.stderr))
    return p.returncode


def _ps(command: str, timeout: float = 30.0):
    return _run(
        ["powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command],
        timeout=timeout,
    )


def task_registered() -> str:
    rc, out = _ps(
        "if (Get-ScheduledTask -TaskName 'ClashNetWatch' -ErrorAction SilentlyContinue)"
        " { (Get-ScheduledTask -TaskName 'ClashNetWatch').State } else { '未注册' }"
    )
    text = _decode(out).strip()
    return f"已注册（{text}）" if text and text != "未注册" else (text or "查询失败")


def cmd_uninstall():
    rc, out = _ps(
        "Unregister-ScheduledTask -TaskName 'ClashNetWatch' -Confirm:$false -ErrorAction SilentlyContinue;"
        " '已移除计划任务'"
    )
    print(_decode(out).strip() or _decode(out))
    return rc


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="Clash Verge 网络感知自动开关")
    parser.add_argument("command", nargs="?", default="run",
                        choices=["run", "status", "doctor", "check", "selftest", "install", "uninstall"])
    parser.add_argument("--dry-run", action="store_true", help="只记录决策，不修改系统状态")
    parser.add_argument("--simulate-ssid", default=None, help="按指定 SSID 模拟当前网络")
    parser.add_argument("--json", action="store_true", help="status 输出 JSON")
    args = parser.parse_args(argv)

    if hasattr(sys.stdout, "reconfigure"):
        try:
            sys.stdout.reconfigure(encoding="utf-8")
        except Exception:
            pass

    cfg = load_config()
    try:
        LOG.max_bytes = int(cfg.get("log_max_bytes") or (1 << 20))
    except (TypeError, ValueError):
        pass
    LOG.console = args.command in ("run", "selftest")

    if args.command == "run":
        try:
            return run_daemon(cfg, args.dry_run, args.simulate_ssid)
        except KeyboardInterrupt:
            LOG.info("收到中断，退出")
            return 0
    if args.command == "status":
        return cmd_status(cfg, args.json)
    if args.command == "doctor":
        return cmd_doctor(cfg)
    if args.command == "check":
        return cmd_check(cfg, args.simulate_ssid)
    if args.command == "selftest":
        return cmd_selftest(cfg)
    if args.command == "install":
        return cmd_install()
    if args.command == "uninstall":
        return cmd_uninstall()
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
