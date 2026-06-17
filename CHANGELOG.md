# Changelog

All notable changes to this project are documented here.

**How releases work:** the top `## [X.Y.Z]` entry below is the *next*
release. Bump it (and write the notes) in a PR; when that PR merges to
`main`, CI tags `vX.Y.Z` and publishes the GitHub Release using this
section as the notes, plus the matching image and Helm chart. You never
create tags by hand.

## [0.2.0] - 2026-06-16

### Features

- ROR `cluster_uid` now carries the cluster's real ROR UUID, resolved from
  `/v2/self` `user.uid`, kept distinct from the human-readable slug in
  `cluster_id`.

### Build

- Upgrade `github.com/NorskHelsenett/ror` to v1.19.4 and the Go base image
  to 1.26.4.

### CI

- CHANGELOG-driven releases: merging a version bump to `main` tags and
  publishes the release; the shared reusable build powers both dev and
  release image builds.
- PR guard (`changelog-check`): every PR into `main` must add a new,
  unreleased `## [X.Y.Z]` section, so a release tag can never be
  overwritten or silently skipped.
