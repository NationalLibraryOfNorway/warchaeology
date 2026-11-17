---
title: Contributing
---

## Build

Use Go 1.27 or later. CI uses Go 1.27.3.

```shell
go build -o warc
```

## Generate release artifacts locally

```shell
goreleaser release --clean --snapshot
```

## Generate CLI docs

```shell
go generate ./docs
```

## Run docs site locally

```shell
cd website
npm install
npm run start
```

## Breaking changes in release notes

GitHub's **Generate release notes** categorizes merged pull requests using
`.github/release.yml`. Apply the `breaking-change` or `Semver-Major` label to
a pull request containing a breaking change; both labels select the existing
**Breaking Changes** category.

Use a descriptive PR title and include migration instructions in its body.
Commit subjects containing `!` and `BREAKING CHANGE:` footers document intent,
but GitHub's built-in release-note generator does not use them to classify PRs.
Review the generated release and add the migration details from
[Migration notes](guides/upgrading.md) before publishing.
