# Contributing

Small fixes are welcome. This is a hobby project, so answers can take a while.

1. Run `gofmt`, `go vet -tags goolm ./...` and `go test -race -tags goolm ./...`.
2. Test data is always made up. Never use real messages, names, photos or screenshots: they contain other parents and children.
3. The bridge stays read-only towards Parro. Do not add calls that change anything there, or ways around a block by Topicus.
4. Each test says in a comment what it protects (`protects`), which wrong result it rejects (`fails_when`) and what it does not prove (`no_claim`).
