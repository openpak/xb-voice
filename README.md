# Xbox Voice Server

OpenPak's Xbox Voice Server.

| | |
| --- | --- |
| Network | Original Xbox |
| Role | Native `xmatch` (service 6) session registry for the dashboard's voice chat |
| Depends on | [`voice`](https://github.com/openpak/voice), [`xb-common`](https://github.com/openpak/xb-common) |
| Used by | The Xbox gateway (`XBGW_BACKENDS`, service 6) |
| Image | `ghcr.io/openpak/xb-voice` |
| Licence | AGPL-3.0-only |

## What it serves

| Endpoint | What |
| --- | --- |
| `POST /xmatch/xmatchhost.srf` | Create a session (all-zero session id) or update one (host only); returns the session id and key |
| `POST /xmatch/xmatchclient.srf` | Find a session by id; returns the host's address, the key and the counts |
| `POST /xmatch/xmatchhostdelete.srf` | Delete a session (host only) |

## Run

Vet and test the module (the image is built and released by CI, see [deploy](docs/deploy.md)):

```sh
GOWORK=off GOPRIVATE='github.com/openpak/*' go vet ./... && go test -race ./...
```

## Docs

- [Protocol](docs/protocol.md)
- [Evidence](docs/evidence.md)
- [Deploy](docs/deploy.md)
- [Notes](docs/notes.md)

## Licence

AGPL-3.0-only. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
