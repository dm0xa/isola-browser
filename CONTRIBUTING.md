# Contributing

Isola is MIT. Issues and pull requests are welcome on
[dm0xa/isola](https://github.com/dm0xa/isola).

## How to run it

Follow [docs/getting-started.md](./docs/getting-started.md). Short version:

```bash
docker compose up --build
```

For a host binary with the UI embedded, see [docs/binaries.md](./docs/binaries.md).
`just build-backend` is the API-only `cmd/server` binary, not the Compose
image.

## Tests

```bash
just test-backend
just test-streamer
cd frontend && npm test
```

`just test-unit` also runs tests for private trees when those are present; on
this clone they are skipped.

## Releasing

1. Add the changes under `## [Unreleased]` in [`CHANGELOG.md`](./CHANGELOG.md).
2. Rename that heading to the version being released
   (`## [X.Y.Z] - YYYY-MM-DD`), leave an empty `## [Unreleased]` above it, and
   commit.
3. Push a `vX.Y.Z` tag.

The `release` workflow derives the binary version from the tag (no source-file
bump is needed), builds the platform archives plus `checksums.txt`, and
publishes the changelog section matching the tag as the release body;
`publish-browser-image` tags the sandbox image with the pushed tag.

## What not to commit

- `.env` and tokens (copy `.env.example`)
- Hashed files under `backend/cmd/single-user/frontend-dist/` (keep `.gitkeep`)
- Binaries listed in `.gitignore`

## License

By contributing you agree that your work is licensed under the MIT license, the same as this repository.
