# easy-terminal

Install:

```sh
npm install -g @lijuneleven/easy-terminal
easy-terminal
```

The CLI starts the local `easy_terminal` service. Pass server flags directly:

```sh
easy-terminal --port 9090
easy-terminal --config-dir /data/easy_terminal
```

`--config-dir` controls the local config and runtime data directory.

The installer downloads the platform binary from GitHub Release first, then falls back to Gitee Release.
It also installs the Codex `notify` and Claude Code `Stop` completion hooks. The CLI retries hook setup on every service start, and `easy-terminal --install-agent-hooks` can run it manually.

The settings page includes an optional one-time environment check for Node.js, the headless browser, writable data storage, Feishu connectivity, and the configured Agent. The result is not saved and does not block setup.
