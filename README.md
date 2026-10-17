# xb-voice

The original Xbox adapter of OpenPak's neutral voice service
([`github.com/openpak/voice`](https://github.com/openpak/voice)). It serves native
**service 6 (`xmatch`)** to the gateway for the dashboard's **VOICE CHAT**
sessions. Evidence and wire formats: workspace `docs/xbox/voice-chat.md`.

How the dashboard's voice chat works (and so what this does): the host console
registers a session with `xmatch` and gets a session id and a key; the host invites
friends through the presence service (already served by xb-social); a friend looks
the session up **by id** and gets the host's address and the key, then connects to
the host console directly, console to console. **The audio never touches a server.**
So xb-voice is a session registry with real authorization, not a media path.

- `POST /xmatch/xmatchhost.srf` (create: all-zero session id; update: known id,
  host only; the host address cannot change) -> 24 bytes: id and key.
- `POST /xmatch/xmatchclient.srf`, find by id only (procedure `0xFFFFFFFF`, one
  integer attribute holding the id) -> the host's `XNADDR`, the key and the counts.
- `POST /xmatch/xmatchhostdelete.srf` (host only).
- The session is mirrored into the voice service as a `NATIVE` session with
  `end_when_owner_leaves`; a lookup is a join (`TRANSPORT_NATIVE_PEER`), decided by
  the account core through the voice service (friend of the host, no block, active
  accounts). A refused lookup gets the same empty result as a session that does not
  exist. The key and the address are only given to an admitted friend.
- Session keys and addresses live in this process only (no database): a restart
  drops resolvable sessions and the dashboard shows "no longer available".

What it does not do, honestly: serve any title other than the dashboard
(`0xFFFE0000`; everything else is `501`), searches other than find-by-id (`501`),
verify the host's `XNADDR` against the address the gateway saw (the feature
`Info` does not carry it yet), or learn that a console left a chat (native
membership is console to console: members stay in the voice service for their
lease, the whole session for `XB_VOICE_SESSION_TTL`). No audio, no codec, no
relay: see the workspace document, sections 3.3 and 6.

Config: `deploy/xb-voice.env.example`. Port: `26117` (feature, ZeroTier address),
`26118` loopback health inside the container. Gateway: add `6=<zerotier>:26117` to
`XBGW_BACKENDS`.

## Repository

- Module `github.com/openpak/xb-voice`, image `ghcr.io/openpak/xb-voice` (static, scratch, uid 10001).
- Shared packages `github.com/openpak/xb-common`, voice contract `github.com/openpak/voice` (both private).
- Build and test: `GOWORK=off GOPRIVATE='github.com/openpak/*' go vet ./... && go test -race ./...`.
- Release: push a `vX.Y.Z` tag; `.github/workflows/release.yml` (tags and manual dispatch only).
- Provenance and licence: AGPL-3.0-only, see `LICENSE` and `NOTICE` (layouts follow Xombie's xmatch codec).
