# Deploy: xb-voice

## Config and ports

Config: `deploy/xb-voice.env.example`. Port: `26117` (feature, ZeroTier address),
`26118` loopback health inside the container. Gateway: add `6=<zerotier>:26117` to
`XBGW_BACKENDS`.

## Repository

- Module `github.com/openpak/xb-voice`, image `ghcr.io/openpak/xb-voice` (static, scratch, uid 10001).
- Shared packages `github.com/openpak/xb-common`, voice contract `github.com/openpak/voice` (both private).
- Build and test: `GOWORK=off GOPRIVATE='github.com/openpak/*' go vet ./... && go test -race ./...`.
- Release: push a `vX.Y.Z` tag; `.github/workflows/release.yml` (tags and manual dispatch only).
