# Iris

Install:

```sh
npm install -g @lijuneleven/iris
iris
```

The CLI starts the local Iris service. Pass server flags directly:

```sh
iris --port 9090
iris --config-dir /data/iris
```

Use `iris status` to list running services. `iris stop` and `iris restart` target the only running service or open a port selector when several are active; pass `<port>` or `all` to select targets directly. Login auto-start is enabled by default and can be disabled in Settings.

Port `8080` keeps its configuration and runtime data in `~/.iris`. Other explicitly selected ports are isolated automatically under `~/.iris/instances/<port>/`, including configuration, database, uploads, session recovery data, and logs. `--config-dir` only overrides the configuration directory, while `IRIS_HOME` changes the instance root.

After the service is ready, Iris opens the local configuration page automatically. First launch uses a dedicated password-setup page with password confirmation; later visits use a dedicated login page, with a secure browser session retained for thirty days.

The installer downloads the platform binary from GitHub Release first, then falls back to Gitee Release.
It also installs the Codex `notify` and Claude Code `Stop` completion hooks together with the Iris Feishu-context Skill for both Agents, and forces Codex `check_for_update_on_startup = false` without replacing other user settings. The CLI retries Agent integration setup on every service start, and `iris --install-agent-hooks` can run it manually.
When a question lacks enough conversation context, that Skill requires the Agent to read the current group's latest messages before asking the user to repeat information.

The settings page includes an optional one-time environment check for Node.js, the headless browser, writable data storage, Feishu connectivity, and the configured Agent. The result is not saved and does not block setup.

On first launch Iris automatically chooses an installed Agent in this order: Codex, then Claude Code. Scanned built-in Agents use fixed commands, while the list supports multiple editable custom Agents; developer-mode Feishu cards use the same list. Restarts resume the exact recorded Codex, Claude Code, or `aiden x codex` session when its session ID is available.

Developer-only assistant mode is off by default. When enabled for a group session, mentioning the configured developer triggers the Agent and the final Feishu card identifies itself as that developer's assistant.
