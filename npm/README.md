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

`--config-dir` controls the local config and runtime data directory.

After the service is ready, Iris opens the local configuration page automatically. First launch uses a dedicated password-setup page with password confirmation; later visits use a dedicated login page, with a secure browser session retained for thirty days.

The installer downloads the platform binary from GitHub Release first, then falls back to Gitee Release.
It also installs the Codex `notify` and Claude Code `Stop` completion hooks together with the Iris Feishu-context Skill for both Agents. The CLI retries Agent integration setup on every service start, and `iris --install-agent-hooks` can run it manually.

The settings page includes an optional one-time environment check for Node.js, the headless browser, writable data storage, Feishu connectivity, and the configured Agent. The result is not saved and does not block setup.

On first launch Iris automatically chooses an installed Agent in this order: Codex, then Claude Code. Developer-mode Feishu cards can switch between installed built-in Agents and the configured custom Agent; built-in Agents always use unattended permission mode.
