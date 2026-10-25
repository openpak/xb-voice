# Protocol: xb-voice

## Flow

How the dashboard's voice chat works (and so what this does): the host console
registers a session with `xmatch` and gets a session id and a key; the host invites
friends through the presence service (already served by xb-social); a friend looks
the session up **by id** and gets the host's address and the key, then connects to
the host console directly, console to console. **The audio never touches a server.**
So xb-voice is a session registry with real authorization, not a media path.

## Endpoints and admission

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
