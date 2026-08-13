# Gocan

`gocan` is a Go stack for communicating with automotive CAN networks
across multiple hardware vendors.

- Start with minimal implementations; validate them, then expand only when needed.
- Keep tests curated, vertical, and multi-stage/lifecycle-focused.
- Keep designs open; compare options through review, feedback, use cases, measurements, and Go language features.
- Keep code `TODO`s for concrete work; put optional future ideas in GitHub issues.
- Go-native scheduling and receive-loop performance appears satisfactory; do not consider provider-native cyclic transmission or receive batching without measured evidence.
- Canonical checks: `nix flake check` (Nix with `nix-command` and `flakes`).
