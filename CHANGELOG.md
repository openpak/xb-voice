# Changelog — xb-voice

## v0.1.1 — 2026-10-18

- Fix: the dashboard's create request carries `flags<<4` in the session-id slot, not
  zeros. A flagged id that is not a known session is now a create (it answered 404 and
  the console showed an Xbox Live error); a known id is still an update, and generated
  ids never take that shape.

## v0.1.0 — 2026-10-17

- First release: native service 6 (xmatch) for the dashboard's voice chat sessions
  (create/update, find by id, delete), mirrored into the neutral voice service,
  authorization through it. No media; other titles and other searches answer 501.
