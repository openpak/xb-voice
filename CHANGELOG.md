# Changelog — xb-voice

## v0.1.2 — 2026-10-18

- Fix: generated session ids carry key type bits `0x80` (online peer) in the first byte.
  The dashboard registers the id with XNetRegisterKey, which accepts only the types
  `0x00`, `0x80` and `0xE0` (`id[0] & 0xE0`) and answers 0x2726 for any other, so about
  six of eight random ids ended in the Xbox Live error. Bytes 1..7 are also never all
  zero, so a generated id can never read as a create.

## v0.1.1 — 2026-10-18

- Fix: the dashboard's create request carries `flags<<4` in the session-id slot, not
  zeros. A flagged id that is not a known session is now a create (it answered 404 and
  the console showed an Xbox Live error); a known id is still an update, and generated
  ids never take that shape.

## v0.1.0 — 2026-10-17

- First release: native service 6 (xmatch) for the dashboard's voice chat sessions
  (create/update, find by id, delete), mirrored into the neutral voice service,
  authorization through it. No media; other titles and other searches answer 501.
