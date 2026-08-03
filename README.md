# gocan

`gocan` is a Go stack for communicating with automotive CAN networks
across multiple hardware vendors.

The project is in its foundation stage. See the
[project specification](GOCAN_SPEC.md) for its scope, implemented architecture,
and development direction.

<!-- TODO: Once the driver set stabilises, add the platform support matrix
here: which drivers exist, which platforms each builds on, and the policy
(one package per driver, neutral translation core buildable everywhere,
native halves platform-gated, virtual as the universal fallback). -->

## Development

The module currently requires Go 1.25 or newer:

```sh
go test ./...
go vet ./...
```

`gocan` is licensed under the [MIT License](LICENSE).
