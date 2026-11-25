# 让 AI 帮你安装并配置

把下面这段话发给能操作本机的 AI（Claude Code / Codex / Qoder / Cursor Agent 等）：

> 帮我在这台 Windows 电脑上安装并配置 Clash NetWatch（https://github.com/King-NO-1/clash-netwatch）：从最新 Release 下载 netwatch-windows-amd64.zip，解压到 D:\Tools\clash-netwatch，用附带的 SHA256SUMS.txt 校验；运行 `netwatch.exe -scan -json` 读出可用 WiFi 并整理成列表给我；然后依次问我「哪些网络要开代理」「其中哪些要自动连接」「哪些绝对不能有代理」，按我的回答写 config.json（其它字段保持默认）；再用 PowerShell 执行 install-task.ps1 注册开机自启，启动 `netwatch.exe -daemon`，最后用 `netwatch.exe -status -json` 确认守护进程在运行。我的代理客户端是 Clash Verge Rev，请先确认它已安装。

## 给 AI 的执行清单

1. **环境检查**
   - `Get-CimInstance Win32_OperatingSystem` 确认是 Windows 10/11；
   - 确认已安装并运行 Clash Verge Rev（`Get-Process clash-verge -ErrorAction SilentlyContinue`）；
   - 解压目录建议非系统盘，路径可含中文，但不要再单独移动 exe。

2. **下载与校验**
   ```powershell
   gh release download --repo King-NO-1/clash-netwatch --pattern "*windows-amd64.zip" --dir $env:TEMP\nw
   # 或直接 curl GitHub Releases 的 zip
   Get-FileHash .\netwatch-windows-amd64.zip -Algorithm SHA256
   ```
   与 Release 的 `SHA256SUMS.txt` 比对（只校验 zip 与 exe 即可）。

3. **扫描网络（只读）**
   ```powershell
   .\netwatch.exe -scan -json
   ```
   返回结构里的 `candidates[]` 包含 `ssid`、`signal`、`profile`、`target`、`auto_connect`、`never_proxy`。
   `profile` 为空表示没手动连过，不能自动连接。

4. **向用户提三个问题**（不要自己替用户决定）
   - 目标网络：连上后要开代理的网络（可多个）；
   - 自动连接：其中哪些要自动切过去（可全部、可子集、可关闭）；
   - 禁止代理：连上必须立刻关代理的网络（校园网/公司网）。**必须拒绝与目标网络重叠的选项。**

5. **写 config.json**（与 exe 同目录，最小可用）
   ```json
   {
     "target_ssids": ["你的热点名"],
     "auto_connect_ssids": ["你的热点名"],
     "never_proxy_ssids": ["校园网名"],
     "fuzzy_match": true,
     "auto_connect": true,
     "enable_system_proxy": true,
     "enable_tun": true
   }
   ```
   其余字段可省略，会自动取默认值。完整字段见 [README 的配置表](README.md#六配置)。
   注意：`enable_system_proxy` 与 `enable_tun` 不能同时为 false。

6. **注册自启并启动**
   ```powershell
   powershell -NoProfile -ExecutionPolicy Bypass -File .\install-task.ps1
   Start-Process .\netwatch.exe -ArgumentList '-daemon' -WorkingDirectory (Get-Location) -WindowStyle Hidden
   Start-Sleep 2
   .\netwatch.exe -status -json
   ```
   期望：`daemon = "running"`，`target = true`（若当前就在目标网络）。

7. **向用户汇报**：目标/自动连接/禁止代理三张名单、当前网络、守护进程状态，并提醒"解压目录不要只移动 exe"。

## 注意事项

- 程序只开关本机已有的 Clash Verge 代理状态，不提供、不修改任何节点或订阅。
- 禁止代理名单务必覆盖学校/公司网络；这是"离开热点后不留代理"的保险。
- 自动连接的前提是目标网络已手动连过一次（Windows 不会凭 SSID 猜密码）。
- 若 AI 没有网络权限，无法下载时，改由用户手动下载 zip，再让 AI 从第 3 步开始。
