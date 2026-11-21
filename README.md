# gh-clone-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/autocv-id/gh-clone-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/autocv-id/gh-clone-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

Anvil Forge — a GitHub-compatible git forge in a single static Go binary: REST API v3, GraphQL, MCP tools, and git smart HTTP (upload-pack protocol v2 + receive-pack v0), all served from one in-memory store with no disk I/O on the hot path. A thin `main.ts` launcher spawns the binary and owns the make.fun lifecycle.

## Run it

```sh
node main.ts
```

The launcher starts the vendored `forge` binary, registers its port with the environment, and waits for the trial to finish. To build the binary from source:

```sh
cd go && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../forge .
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=ehvqyq86&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
