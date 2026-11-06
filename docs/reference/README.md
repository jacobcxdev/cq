# Frozen proxy review authority

These two files preserve the exact reviewed proxy blueprint and its canonical review record:

- [Proxy resilience and routing blueprint](2026-08-13-proxy-resilience-and-routing-blueprint.md)
- [Canonical review record](2026-08-13-proxy-resilience-and-routing-blueprint.review.json)

They are immutable verification inputs, rather than the current onboarding or operating guide. Their original document and review digests remain pinned in `internal/proxy/canonical_jcs.go`; the review baseline is `9fe30df8d4101f69084d6487740ed324a5d0b59d`.

The review record retains its original logical `blueprint_path` under `docs/superpowers/specs`. That string is part of the signed-off canonical authority and its digest. The physical files moved here without changing their bytes. Verification tools read these durable paths; local planning files under `docs/superpowers` are ignored and are not required by a clean checkout.

On macOS, run `scripts/verify-blueprint-review` from the repository root. The proxy race tests also validate the frozen authority and reject altered documents, records and symlinks.
