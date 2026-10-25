# Notes: xb-voice

## Overview

The original Xbox adapter of OpenPak's neutral voice service
([`github.com/openpak/voice`](https://github.com/openpak/voice)). It serves native
**service 6 (`xmatch`)** to the gateway for the dashboard's **VOICE CHAT**
sessions. Evidence and wire formats: internal research notes.

## Session state

- Session keys and addresses live in this process only (no database): a restart
  drops resolvable sessions and the dashboard shows "no longer available".

## Status

What it does not do, honestly: serve any title other than the dashboard
(`0xFFFE0000`; everything else is `501`), searches other than find-by-id (`501`),
verify the host's `XNADDR` against the address the gateway saw (the feature
`Info` does not carry it yet), or learn that a console left a chat (native
membership is console to console: members stay in the voice service for their
lease, the whole session for `XB_VOICE_SESSION_TTL`). No audio, no codec, no
relay: see the workspace document, sections 3.3 and 6.

## Provenance and licence

- Provenance and licence: AGPL-3.0-only, see `LICENSE` and `NOTICE` (layouts follow Xombie's xmatch codec).
