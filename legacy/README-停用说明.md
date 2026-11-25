# 这个目录的代码已停用，不要运行

这里的 `netwatch.py` 是 v1（Python 轮询版），已被上一级的 **netwatch.exe（Go 版）** 取代。

如果误运行 `python netwatch.py run`，它会和 Go 版同时抢 Clash Verge 的 TUN 与系统代理，
导致两边不停互相覆盖（一个开、另一个关），表现为网络反复断。

保留原因：只在需要回滚时参考。回滚步骤：停掉计划任务 `ClashNetWatch`，
再用 `pythonw.exe netwatch.py run` 手动启动（旧版配置字段与新版的 `config.json` 不兼容，
需要自己按旧格式重写一份）。
