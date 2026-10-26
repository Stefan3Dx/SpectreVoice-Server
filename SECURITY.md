# Security

## Reporting a vulnerability

Write to **security@spectrevoice.com** (or open a private
[security advisory](https://github.com/Stefan3Dx/SpectreVoice-Server/security/advisories/new)). Please don't open a
public issue for it. Say what's affected, how to reproduce it and the version (`spectrevoice-server version`).

- You get an answer within **3 working days**.
- A fix is released as soon as it's ready; the advisory is published with it, crediting you if you wish.
- Please give us **90 days** before publishing details, less once a fix is out.

## Supported versions

The latest release. Update with `sudo bash install.sh --update`.

## What we do (EU Cyber Resilience Act)

- An actively exploited vulnerability or a severe incident is reported to the national CSIRT (DNSC, Romania) and ENISA
  through the Single Reporting Platform: an early warning within **24 hours**, a notification within **72 hours**, a
  final report within **14 days** after a fix (one month for incidents).
- Server owners are told through the release notes and the apps' update notice.
- Releases are built by CI from this repository, with SHA-256 checksums.
