#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""判定逻辑与失败路径测试（不依赖真实网络，可在任意环境跑）。"""
import sys
import time
from pathlib import Path

APP = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(APP))

import netwatch as nw  # noqa: E402

# 测试自己的日志单独放，别污染生产日志
nw.LOG.path = APP / "logs" / "test_logic.log"
nw.LOG.console = False
if nw.LOG.path.exists():
    nw.LOG.path.unlink()

CFG = nw.load_config()
results = []


def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))


def obs(ssid, state="connected"):
    return {"state": state, "ssid": ssid, "iface": "WLAN", "source": "test", "note": ""}


# ---- 1. match_network 归一化
check("大小写+空格差异命中", nw.match_network("MyPhoneMagic6  Pro", ["MyPhonemagic6 pro"], True))
check("连字符归一化", nw.match_network("Campus-WiFi", ["campus wifi"], True))
check("前缀容错 pro2 -> pro", nw.match_network("MyPhonemagic6 pro2", ["MyPhone"], True))
check("模糊关闭时不误命中", nw.match_network("MyPhonemagic6 pro2", ["MyPhone"], False) is None)
check("完全不同的网络不命中", nw.match_network("CMCC-5G", ["MyPhone"], True) is None)
check("空 SSID 不命中", nw.match_network(None, ["Campus-WiFi"], True) is None)

# ---- 2. decide 决策矩阵
check("热点 -> ON", nw.decide(obs("MyPhone"), CFG)[0] == "on")
check("热点变体 -> ON", nw.decide(obs("MyPhonemagic6 pro2"), CFG)[0] == "on")
check("校园网 -> OFF", nw.decide(obs("Campus-WiFi"), CFG)[0] == "off")
check("校园网 5G 变体 -> OFF", nw.decide(obs("Campus-WiFi-5G"), CFG)[0] == "off")
check("陌生网络 -> 默认 OFF", nw.decide(obs("Starbucks"), CFG)[0] == "off")
check("断开连接 -> OFF", nw.decide(obs(None, "disconnected"), CFG)[0] == "off")
check("探测失败 -> unknown（不动作）", nw.decide(obs(None, "unknown"), CFG)[0] == "unknown")

# ---- 3. 内核不可用时不盲目开代理
real_find = nw.find_core_pipes
try:
    nw.apply_target("off", CFG, reason="test:baseline")
    nw.find_core_pipes = lambda: []          # 模拟内核/管道全部消失
    r = nw.apply_target("on", CFG, reason="test:core-down")
    cur = nw.runtime_state(CFG)
    check("内核缺失时 apply 报失败", r["ok"] is False, str(r["steps"]))
    check("内核缺失时不会开系统代理", cur["sysproxy"] is False, f"sysproxy={cur['sysproxy']}")
finally:
    nw.find_core_pipes = real_find

# ---- 4. TUN 开启失败时回滚系统代理
real_set_tun = nw.set_tun
try:
    nw.set_tun = lambda enable: (False, "模拟 TUN 失败")
    r = nw.apply_target("on", CFG, reason="test:tun-fail")
    cur = nw.runtime_state(CFG)
    check("TUN 失败时 apply 报失败", r["ok"] is False, str(r["steps"]))
    check("TUN 失败时系统代理已回滚", cur["sysproxy"] is False, f"sysproxy={cur['sysproxy']}")
finally:
    nw.set_tun = real_set_tun

# ---- 5. 管道调用超时与熔断（防止内核卡死拖住守护进程）
t0 = time.time()
r = nw._call_with_timeout(lambda: time.sleep(3) or ("late", "late"), 0.5)
dt = time.time() - t0
check("超时调用按时返回", r == (None, None), f"{dt:.2f}s")
check("超时调用不阻塞主线程", dt < 1.5, f"{dt:.2f}s")

try:
    nw._call_with_timeout(lambda: (_ for _ in ()).throw(OSError("boom")), 2)
    raised = False
except OSError:
    raised = True
check("异常会透传给调用方", raised)

nw._pipe_note_timeout()
check("单次超时不熔断", not nw._pipe_cooldown_active())
nw._pipe_note_timeout()
check("连续两次超时进入熔断", nw._pipe_cooldown_active())
status, _ = nw.core_api("GET", "/version")
check("熔断期内直接返回不可用", status is None)
nw._PIPE_STATE["cooldown_until"] = 0.0
nw._PIPE_STATE["timeouts"] = 0
check("熔断解除后能恢复访问", nw.core_version() is not None)

# ---- 复原
nw.apply_target("on", CFG, reason="test:restore")

failed = [r for r in results if not r[1]]
for name, ok, detail in results:
    print(f"  [{'PASS' if ok else 'FAIL'}] {name} {detail}")
print(f"== 逻辑测试：{len(results) - len(failed)}/{len(results)} 通过 ==")
sys.exit(0 if not failed else 1)
