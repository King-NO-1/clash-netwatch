"""测 TUN / 系统代理 的实际启停耗时（毫秒级）。"""
import json
import os
import time
import winreg

import netwatch as nw


def patch(enable: bool) -> float:
    tun = nw.get_tun_config()
    t0 = time.perf_counter()
    body = dict(tun, enable=enable)
    st, _ = nw.core_api("PATCH", "/configs", {"tun": body})
    t_cmd = (time.perf_counter() - t0) * 1000
    # 高频轮询（20ms）
    while (time.perf_counter() - t0) < 5:
        now = nw.get_tun_config()
        if now is not None and bool(now.get("enable")) == enable:
            return t_cmd, (time.perf_counter() - t0) * 1000
        time.sleep(0.02)
    return t_cmd, -1


def sysproxy(on: bool) -> float:
    t0 = time.perf_counter()
    nw.set_sysproxy(on, 7897)
    return (time.perf_counter() - t0) * 1000


if __name__ == "__main__":
    nw.LOG.console = False
    # 预热一次，排除首次管道连接成本
    nw.get_tun_config()

    print("== TUN 关闭 ==")
    for i in range(3):
        cmd, total = patch(False)
        print(f"  第{i+1}次: 下发 {cmd:5.0f}ms  生效 {total:6.0f}ms")
        time.sleep(0.5)
    print("== TUN 开启 ==")
    for i in range(3):
        cmd, total = patch(True)
        print(f"  第{i+1}次: 下发 {cmd:5.0f}ms  生效 {total:6.0f}ms")
        time.sleep(0.5)

    print("== 系统代理 ==")
    for i in range(3):
        off = sysproxy(False)
        on = sysproxy(True)
        print(f"  第{i+1}次: 关 {off:4.0f}ms  开 {on:4.0f}ms")

    print("== netsh 取 SSID ==")
    for i in range(3):
        t0 = time.perf_counter()
        nw.probe_wlan()
        print(f"  第{i+1}次: {(time.perf_counter()-t0)*1000:.0f}ms")
