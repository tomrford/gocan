# `tomrford/gocan`

## Prerelease work

This temporary plan lists work intended before the first stable release. Source
and package documentation define implemented behaviour. Delete this file after
the first stable release.

### Live trace writing

Decide whether the first stable release needs to write records while capture is
active. If it does, define ownership, backpressure, writer-failure, and shutdown
behaviour, then implement the minimum useful path.

### Semantic UDS

Add typed standard services, session state, and OEM extension points on top of
the raw `uds` request and response values.

### CANdela CDD

Validate the current resolved DID catalog against real CANdela CDD files. Add
DID payload codecs, then expand the catalog for named sessions, security
levels, routines, and timing as semantic UDS gains those operations. Report
unsupported selected records rather than silently discarding their fields.
