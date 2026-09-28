# agentboard

A live board of your coding-agent sessions: which ones are working, which are
waiting on you, and the decisions they've asked you to make. One static Go
binary, SQLite on disk, a plain web page. No Node, no cgo.

## Install

```sh
./build.sh                                  # dist/agentboard-<os>-<arch>[.exe]
install -m 0755 dist/agentboard-darwin-arm64 /usr/local/bin/agentboard
```

The command is `agentboard`. There is no short alias. Put it on `PATH` on
every machine that runs agents, because the hooks call it by name.

## Serve

```sh
agentboard serve \
  --listen 127.0.0.1:8790 \
  --listen 192.168.64.1:8790 \
  --listen 192.0.2.10:8790 \
  --listen 198.51.100.7:8790
```

- `127.0.0.1` is this machine. `192.168.64.1` is the VM bridge. Both accept
  posts from agents (ingest).
- `192.0.2.10` (LAN) and `198.51.100.7` (VPN) are read-only: open the page
  from a phone or laptop there, but posts get 403.
- Ingest is allowed from `127.0.0.0/8`, `::1/128` and `192.168.64.0/24` by
  default. `--ingest-from CIDR` (repeatable) replaces that list.
- An address that isn't up yet (the VM is stopped, the VPN is down) is
  retried every 30 s. It is never fatal.
- The database defaults to `<user config dir>/agentboard/agentboard.db`;
  `--db PATH` overrides it.

### Run it under relay

```sh
relay service register --name agentboard --command agentboard \
  --args serve \
  --args=--listen=127.0.0.1:8790 \
  --args=--listen=192.168.64.1:8790 \
  --url http://127.0.0.1:8790 --autostart
```

`--args` is repeatable, one argument each. Use the `--args=--flag=value` form
for arguments that start with a dash.

## Report from a VM (devbox)

Inside the VM, point the client at the host's bridge address and name the
machine:

```sh
export AB_URL=http://192.168.64.1:8790
export AB_MACHINE=devbox
```

`AB_NAME` and `AB_RUN` optionally label the session and the run it belongs to.

## Claude Code hooks

Merge this into `~/.claude/settings.json` (also in
`examples/claude-settings.json`):

```json
{
  "hooks": {
    "SessionStart":       [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}],
    "UserPromptSubmit":   [{"hooks": [{"type": "command", "command": "agentboard hook", "async": true}]}],
    "PreToolUse":         [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}],
    "PostToolUse":        [{"hooks": [{"type": "command", "command": "agentboard hook", "async": true}]}],
    "PostToolUseFailure": [{"hooks": [{"type": "command", "command": "agentboard hook", "async": true}]}],
    "Notification":       [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}],
    "Stop":               [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}],
    "SessionEnd":         [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}]
  }
}
```

`agentboard hook` never writes stdout and always exits 0, so it can't add to
the model's context or block a session. It sends only derived labels
(machine, project, branch, repo, issue), never paths, prompts or tool input.

## Post without the CLI

```sh
curl -sS -X POST http://127.0.0.1:8790/api/log \
  -H 'Content-Type: application/json' \
  -d '{"ctx":{"machine":"devbox","project":"acme"},"text":"nightly started"}'
```

## CLI

`agentboard <subcommand>`. Flags may go anywhere; `--` ends flags; both
`--f v` and `--f=v` work.

| Subcommand | Does | Prints on success |
|---|---|---|
| `ask <question…> --rec <text> [--kind question\|review\|notify]` | post a decision (`--rec` optional for notify) | `<id>` |
| `answer <id> <text…>` | answer a decision | `answered <id>` |
| `dismiss <id>` | dismiss a decision | `dismissed <id>` |
| `state <note…>` | set this session's note (`""` clears; needs `CLAUDE_CODE_SESSION_ID`) | `ok` |
| `log <text…>` | append a log line | `ok` |
| `item <repo>#<N> [--title T] [--pr N] [--state S] [--tier T]` | upsert a ledger item | `<key>` |
| `went well\|less <text…>` | add a retro note | `ok` |
| `run start <name>` | start or reopen a run | `run <name> started` |
| `run end [<name>]` | end a run | `run <name> ended` |
| `meter [--run R] [--open-start N] [--open-now N] [--filed N] [--closed N]` | set run meter values | `ok` |
| `prune --ended-older DUR \| --ended-all \| --quiet DUR` | clear old sessions (Go duration, e.g. `24h`) | `cleared <n>` |
| `hook` | Claude Code hook adapter (stdin) | nothing, ever |
| `serve …` | run the board server | nothing; logs to stderr |
| `help` | usage | usage |

**Exit codes**

| Code | When |
|---|---|
| 0 | success, or **fail-soft**: server down, timeout, 5xx, or 403. stderr says `agentboard: not recorded: <reason>` and stdout is empty. |
| 1 | the server rejected the request: 400, 404, 409 or 413 |
| 2 | local usage error, before any network call |

Every client call finishes within 250 ms, so a stopped board never slows an
agent down. Scripts should treat exit 0 with empty stdout from `ask` as "not
recorded".
