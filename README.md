# Iris

Iris 是一个运行在本机的飞书个人 AI 助理。它把飞书机器人、真实终端和本地 Agent 连接起来：每个会话都会自动启动一个 Agent，既可以服务开发者自己的工作流，也可以先接待通过飞书联系开发者的人。

## 主要能力

- Agent 自动选择：首次使用优先选择本机 Codex，其次选择 Claude Code；未检测到内置 Agent 时再填写自定义命令。
- 联系人接待：他人私聊机器人后，自动创建“与某某的对话”群聊，并加入联系人、开发者和机器人。
- 会话复用：同一联系人后续私聊会继续进入已有群聊和 Agent 会话。
- 当前群上下文：Agent 可自动识别当前会话绑定的飞书群，并按需读取最近 1～100 条群消息，无需用户提供群 ID。
- 艾特模式：联系人群聊和机器人直接加入的群聊默认需要 `@Iris` 才触发回复。
- 开发者模式：只有配置的开发者可以开关；开启后显示目录、模型、推理等级、Agent 重启、终端快捷键和自定义快捷键。
- Agent 切换：开发者模式卡片只展示本机已安装的 Codex、Claude Code 和已配置的自定义 Agent，并在确认旧 Agent 退出后完成切换。
- Agent 恢复：Iris 重启时会先检查并升级正在使用的 Codex、Claude Code，再恢复会话；其他 Agent 可通过卡片复用已配置的启动命令重启。
- 工作目录：每个会话默认使用 `~/Iris_Workspace/会话名称`，并可从飞书卡片切换到额外配置的项目目录。
- 设置保护：首次使用引导设置密码，也允许在明确确认风险后跳过；支持在设置页修改和本机重置。
- 环境检测：可在设置页按需检查 Node.js、Headless 浏览器、数据目录、飞书连接和 Agent 环境；结果仅即时展示，不保存也不影响主流程。
- Web 终端：保留真实终端、长文本输入、图片粘贴、快捷命令和会话管理能力。

## 开发运行

环境要求：

- Go 1.25 或兼容版本
- macOS 或 Linux
- 可交互 Shell；优先使用系统配置的 `$SHELL`，并自动回退到 Bash、sh、zsh 或 Fish
- 已安装并登录所选 Agent，例如 Codex CLI 或 Claude Code

启动服务：

```sh
make run
make build
./iris
```

默认监听 `8080`。也可以指定端口或独立配置目录：

```sh
go run ./cmd --port 8082
go run ./cmd --config-dir /data/iris
IRIS_CONFIG_DIR=/data/iris go run ./cmd
```

首次打开页面时，依次完成设置密码和 Agent 选择。之后在设置页配置飞书应用、开发者 open_id、工作目录和自定义快捷键。

Iris 启动时会自动安装并维护 Codex `notify`、Claude Code `Stop` Hook，以及两者用于识别当前飞书群的 Skill，保留用户已有配置。安装包也会在安装完成后执行同样的配置；如当时没有写入权限，首次启动会自动重试。可手动执行 `iris --install-agent-hooks` 重新安装。

## 飞书配置

设置页支持扫码创建或配置飞书自建应用。联系人助理流程需要机器人具备消息、群聊、卡片和用户基本信息权限，其中包括：

- 消息接收与发送
- 群聊创建与成员管理
- 卡片读取、写入和回调
- `contact:user.base:readonly`

如果希望群聊中不 `@Iris` 也能触发回复，还需在飞书后台开通 `im:message.group_msg` 并发布应用版本。

## 飞书使用方式

- 普通私聊：自动建立或复用联系人群聊，消息交给 Agent 处理。
- `开始 会话名`：创建开发会话，默认开启开发者模式并关闭艾特模式。
- 直接把机器人拉进群：自动创建绑定会话，默认开启艾特模式、关闭开发者模式。
- 卡片“开发者模式”：仅配置的开发者可以切换，其他已显示按钮可由普通群成员使用。

## 设置密码

设置页可修改密码。忘记密码时，在运行 Iris 的本机执行：

```sh
go run ./cmd --reset-settings-password
```

跳过密码会让同一网络中能访问 Iris 页面的人有机会修改飞书凭证、Agent 命令和工作目录，请只在可信环境使用。

## 运行数据

默认数据目录：

- `~/.iris/conf/config.local.json`
- `~/.iris/iris.db`
- `~/.iris/data/uploads/`
- `~/.iris/log/iris.log`

迁移旧部署时仍可读取旧版环境变量，但新配置统一使用 `IRIS_HOME` 和 `IRIS_CONFIG_DIR`。

## 验证

```sh
make test
make test-browser
make test-codex-tui
make test-claude-hook
```

浏览器 E2E 会验证首次密码、Agent 初始化和真实终端链路；Codex TUI E2E 会验证 Iris 自动启动 Codex、单轮内容截取以及模型和推理菜单；Claude Hook E2E 会通过真实 Claude 回合验证 Stop Hook 的最终回复回调。
