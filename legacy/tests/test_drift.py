#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""漂移自愈 + 单实例 测试：在守护进程运行时从外部改掉 TUN，看它会不会自己拉回来。"""
import json
import os
import subprocess
import sys
import time
from pathlib import Path

APP = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(APP))

import netwatch as nw  # noqa: E402

nw.LOG.path = APP / "logs" / "test_drift_harness.log"
nw.LOG.console = False

TMP_CFG = APP / "tests" / "config.drift.json"


def ps(command: str) -> str:
    p = subprocess.run(["powershell", "-NoProfile", "-NonInteractive", "-Command", command],
                       capture_output=True)
    return nw._decode(p.stdout).strip()


def stop_prod_daemon() -> None:
    """生产实例持有互斥体，测试前先停掉，结束后再拉起来。"""
    ps("Stop-ScheduledTask -TaskName 'ClashNetWatch' -ErrorAction SilentlyContinue")
    ps("Get-CimInstance Win32_Process -Filter \"Name='pythonw.exe'\" | "
       "Where-Object { $_.CommandLine -like '*netwatch.py*' } | "
       "ForEach-Object { Stop-Process -Id $_.ProcessId -Force }")
    time.sleep(1)


def start_prod_daemon() -> None:
    ps("Start-ScheduledTask -TaskName 'ClashNetWatch' -ErrorAction SilentlyContinue")


def runtime():
    return nw.runtime_state(nw.load_config())


def main() -> int:
    base = json.loads((APP / "config.json").read_text(encoding="utf-8"))
    base.update({"drift_check_seconds": 4, "min_reapply_seconds": 2, "confirm_polls": 2, "poll_seconds": 2})
    TMP_CFG.write_text(json.dumps(base, ensure_ascii=False, indent=2), encoding="utf-8")

    print("  停止生产实例…")
    stop_prod_daemon()

    env = dict(os.environ, NETWATCH_CONFIG=str(TMP_CFG),
               NETWATCH_LOG=str(APP / "logs" / "test_drift_daemon.log"))
    nw.apply_target("on", nw.load_config(), reason="test:prepare")

    log_path = APP / "logs" / "drift_test.log"
    if log_path.exists():
        log_path.unlink()
    state_path = APP / "state.json"
    if state_path.exists():
        state_path.unlink()  # 避免读到上一轮残留状态
    with log_path.open("w", encoding="utf-8") as logf:
        proc = subprocess.Popen([sys.executable, str(APP / "netwatch.py"), "run"],
                                env=env, stdout=logf, stderr=subprocess.STDOUT)
        results = []
        try:
            # 等它完成首轮判定与应用（state.json 出现 applied=on）
            t0 = time.time()
            initial_done = False
            while time.time() - t0 < 25:
                try:
                    st = json.loads((APP / "state.json").read_text(encoding="utf-8"))
                    if st.get("applied") == "on" and st.get("ok"):
                        initial_done = True
                        break
                except (OSError, json.JSONDecodeError):
                    pass
                time.sleep(1)
            results.append(("首轮应用完成", initial_done, "state.json applied=on"))
            time.sleep(2)

            # 单实例检查：再起一个应该立刻退出
            second = subprocess.run([sys.executable, str(APP / "netwatch.py"), "run"], env=env,
                                    capture_output=True, timeout=30)
            out2 = nw._decode(second.stdout)
            results.append(("单实例互斥", "已有实例在运行" in out2, out2.strip()[:80]))

            # 外部破坏：把 TUN 关掉，模拟用户在 Verge 里手改
            st, _ = nw.core_api("PATCH", "/configs",
                                {"tun": dict(nw.get_tun_config(), enable=False)})
            time.sleep(1.5)
            broken = nw.get_tun_config().get("enable")
            results.append(("外部破坏生效(TUN=off)", broken is False, f"tun={broken}"))

            # 等自愈
            t0 = time.time()
            healed = False
            while time.time() - t0 < 60:
                if nw.get_tun_config().get("enable") is True:
                    healed = True
                    break
                time.sleep(1)
            results.append(("漂移自愈恢复 TUN", healed, f"{time.time() - t0:.0f}s 内恢复"))
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()

    text = log_path.read_text(encoding="utf-8")
    results.append(("日志记录了漂移", "运行态漂移" in text, ""))
    results.append(("日志记录了自愈", "漂移自愈" in text, ""))

    ok = True
    for name, passed, detail in results:
        print(f"  [{'PASS' if passed else 'FAIL'}] {name} {detail}")
        ok &= passed
    print(f"== 漂移测试：{'全部通过' if ok else '有失败'} ==")
    nw.apply_target("on", nw.load_config(), reason="test:cleanup")
    print("  重启生产实例…")
    start_prod_daemon()
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
