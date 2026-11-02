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

## Title matchmaking

Any title id other than the dashboard's gets its own pool. The title is the pool key:
a session is only found, searched, updated or deleted under the title it was hosted
under, and a request whose body title differs from the connection's title (as bound by
the gateway) is `501`. Title sessions are public: no friend check, not mirrored into
`voice`, and never visible to the dashboard path (and the dashboard's chats never
appear in a title's search).

| Request | Behaviour |
| --- | --- |
| host, zero or flagged id | create; replaces the same host's older session for that title; reply id + key; the attribute list is stored verbatim |
| host, known id | update counts and attributes (host only `403`, address cannot change `409`, unknown/expired `404`); counts as the refresh |
| client, find-by-id (`0xFFFFFFFF`, one int attribute) | that session if it is in the title's pool |
| client, anything else | search: the title's sessions where every non-null search attribute equals a session attribute (same tag, type, value) and `public_open >= num_users`; newest first, at most 10 |
| hostdelete | host only; the body title must be the session's title (`404` otherwise) |
| expiry | a session not created/updated by its host for 15 min (`XB_VOICE_TITLE_TTL`, min 1m) is dropped; searches do not keep it alive |

From Xombie (`libs/xombie-matchmaking`, read-only reference, clean-room Go here): the
message layouts, per-title sessions, the 10-result limit. Inferred, not observed: the
procedure index is ignored (real titles used title-defined server procedures), the
attribute equality and wildcard rules, the `num_users` check, the TTL, and the
assumption that the host refreshes by re-posting its session. Xombie itself leaves
attribute and user matching as a TODO.

## Scope and limits

Serves native service 6 (`xmatch`) to the gateway for the dashboard's VOICE CHAT sessions; it is the original Xbox adapter of [`voice`](https://github.com/openpak/voice). Session keys and addresses live in this process only (no database): a restart drops resolvable sessions and the dashboard shows "no longer available". The dashboard (`0xFFFE0000`) is served find-by-id only (other searches `501`); title sessions also live in this process only. It does not verify the host's `XNADDR` against the address the gateway saw, and does not learn that a console left a chat (members stay in the voice service for their lease, the whole session for `XB_VOICE_SESSION_TTL`). No audio, codec or relay.
