# Gocan

`gocan` is a Go stack for communicating with automotive CAN networks
across multiple hardware vendors.

- The module is pre-v1.0 (0ver); prefer clean API simplifications over preserving backwards compatibility.
- Keep tests curated, vertical, and multi-stage/lifecycle-focused.
- Never write unit tests after you write code.
- Highly prefer E2E or integration tests as the sole testing mechanism. Use them to verify complex features work. At the end of those tests, produce a verifiable and repeatable artifact.
- If you must test a system in isolation, first write down all the ways it could fail, then write the code.
- A test that breaks under a behavior-preserving refactor is asserting implementation, not behavior. Do not add it.
- Never delete or weaken a failing test to make the suite pass. Fix the code, or ask.
- Go-native scheduling and receive-loop performance appears satisfactory; do not consider provider-native cyclic transmission or receive batching without measured evidence. Exception: NI-XNET polls bounded batches because its native API has no receive-ready event.
- To cut a release once `main` CI is green, push an annotated `vX.Y.Z` tag (`git tag -a vX.Y.Z && git push origin vX.Y.Z`), which triggers `.github/workflows/release.yml` to publish the GitHub release; optionally curate the notes afterward with `gh release edit vX.Y.Z` (or run `gh release create vX.Y.Z --verify-tag --generate-notes` if GitHub drops the tag event).

## Reference sources

Clone upstream sources into `.repos/` (gitignored) only when needed; prefer a shallow clone at the noted ref.

- python-can: https://github.com/hardbyte/python-can (incl. `can/io/mf4.py`)
- python-can-j1939: https://github.com/juergenH87/python-can-j1939 at `01aba95c` (J1939 TP timers/layouts)
- Linux CAN: https://github.com/linux-can/can-utils; J1939 `transport.c` in the kernel at `9f0346dc`
- asammdf: https://github.com/danielhrisca/asammdf at `8.7.2` (MDF bus logging)
- cantools, can-isotp, udsoncan: https://github.com/cantools/cantools, https://github.com/pylessard/python-can-isotp, https://github.com/pylessard/python-udsoncan
- DBC parsers: crates `dbc-rs` 0.10.0 and `can-dbc-pest` 0.8.0, https://github.com/oxibus/can-dbc, https://github.com/einride/can-go
- NI-XNET: https://github.com/ni/nixnet-python
- cantraceviewer: https://github.com/tomrford/cantraceviewer
