# 从 ARTEX-God 迁移到 Vestrail

仓库与项目名改为 `Vestrail`，Go 模块为 `github.com/Alexander4731/Vestrail`，命令入口为 `./cmd/vestrail`。首个使用新名称的正式版本为 `0.3.25`，发布包由 GitHub Actions 从对应标签构建。

## 兼容行为

| 项目 | 新行为与兼容范围 |
| --- | --- |
| 程序与包 | `vestrail` / `vestrail.exe`，`vestrail-<版本>-<os>-<arch>.zip`；Windows 启动器为 `vestrail-launcher.exe` |
| 管理员 | 界面使用 `Vestrail`，仍接受 `ARTEX` 登录同一账户，密码保持原值 |
| 会话 | 新 JWT 使用 `Vestrail` subject、`vestrail_token` HttpOnly cookie；旧签名会话仍按到期时间与 session epoch 校验，登录或退出时清理旧 cookie |
| 环境变量 | `VESTRAIL_` 优先，未设置时读取对应的 `ARTEX_`；显式空值也覆盖旧值，防止旧令牌或开关意外重新生效 |
| 配置与数据 | 继续读取原 `config.json`、`jwt.key`、`data/`、`skills/`；不重命名数据库、角色、任务目录，不覆盖自定义提示词 |
| Windows 便携版 | 新安装默认使用 `vestrail` 数据库和角色；已有配置完整保留，启动时屏蔽新旧外部 DSN，继续连接包内数据库 |
| 浏览器偏好 | 保留原本地存储键，列表排序、筛选、折叠和输入习惯继续有效 |
| 更新器 | 优先识别 Vestrail 包，也能读取历史 `artex-*` 包及可执行文件；混合或重复的可执行文件会被拒绝，SHA-256 检查保持启用 |
| 审计 | 新单用户操作来源为 `human:Vestrail`，历史 `human:ARTEX` 保留；不表示已实现多用户身份 |

运行配置包括 `CONFIG`、`PG_DSN`、`SKILL_DIR`、`LLM_PROVIDER`、`LLM_MODEL`、`LLM_BASE_URL`、`LLM_PROXY`、`LLM_STREAM`、`BTW_MAX_OUTPUT_TOKENS`、`NOTIFY_ALLOW_LOCAL` 与 `UPDATE_GITHUB_TOKEN` 后缀。构建脚本也兼容旧 `ARTEX_` 参数。

## 已有部署的首次切换

1. 停止旧程序及其守护脚本，备份一致的数据库、`config.json`、`jwt.key`、`data/`、自定义 `skills/` 与程序。数据库备份和文件备份必须对应同一停机时间点。
2. 下载 `0.3.25` 或更新版本的 Vestrail 包，按 Release 的 `SHA256SUMS` 验证。历史 ARTEX Release 附件保留原名称，不会重新打包历史版本。
3. 将新程序和 `start.sh` / `start.bat` 放在原部署目录，保留配置与数据。Windows 便携部署还需更新 `vestrail-launcher.exe`、`start-engine.bat`，按新包提供的版本更新运行库；不要用示例配置覆盖原 `config.json`，也不要另建空 `data/` 后误当作原库。
4. 修改自定义 systemd、Docker Compose 或其他进程管理配置中的程序路径。新守护脚本优先启动 `vestrail`，缺少时仍可启动 `artex`。只使用一个守护入口，防止旧进程同时运行。
5. 启动并检查 `/api/health` 的 `service` 为 `vestrail`，确认原密码可用、任务与审计记录仍在，再切换监控中的服务标识。保留备份直到验收完成。

旧版本内置更新器只认识 `artex-*` 发布包，首次切换请按上述步骤手动完成；Vestrail 后续版本可继续使用页面更新。更新暂存与回滚路径由当前可执行文件名推导。不要在存在 `.new` / 升级标记的未完成更新中直接改文件名。

本批集成还包含 v5 归档，旧版本不一定能读取新归档。回退整个应用时应恢复配套数据库与文件备份，页面程序回滚不会回退数据库结构。能力边界见 [INTEGRATION_STATUS.md](INTEGRATION_STATUS.md)。

## 源码仓库与保留的旧名

```sh
git remote set-url origin https://github.com/Alexander4731/Vestrail.git
git switch main
git pull --ff-only
```

本地检出目录可以继续使用旧名。ARTEX 上游链接、历史发布附件和研究记录、第三方补丁归属、会话去重标记及 `skills/artex-auth-rce` 检测规则保留原名；后者识别的是 ARTEX 产品，不应替换为 Vestrail 指纹。原许可证保留。
