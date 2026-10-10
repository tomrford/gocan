# Gocan

`gocan` is a Go stack for communicating with automotive CAN networks
across multiple hardware vendors.

- Start with minimal implementations; validate them, then expand only when needed.
- Keep tests curated, vertical, and multi-stage/lifecycle-focused.
- Never write unit tests after you write code.
- Highly prefer E2E or integration tests as the sole testing mechanism. Use them to verify complex features work. At the end of those tests, produce a verifiable and repeatable artifact.
- If you must test a system in isolation, first write down all the ways it could fail, then write the code.
- A test that breaks under a behavior-preserving refactor is asserting implementation, not behavior. Do not add it.
- Never delete or weaken a failing test to make the suite pass. Fix the code, or ask.
- Keep designs open; compare options through review, feedback, use cases, measurements, and Go language features.
- Keep code `TODO`s for concrete work; put optional future ideas in GitHub issues.
- Go-native scheduling and receive-loop performance appears satisfactory; do not consider provider-native cyclic transmission or receive batching without measured evidence. Exception: NI-XNET offers no receive-ready event, so its driver polls and reads bounded batches. Native reads are serialized with transmission by default; Config.NIXNETConcurrentIO opts into reading outside the transmit lock, because a measured native read held that lock for 38 ms while sends waited (October 2026). Each batch is published under one lock in either mode.
- Canonical checks: `go test -race ./...`, `go vet ./...`, and `gofmt -l .`.
