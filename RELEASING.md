# Releasing kubectl-nv

A release is the GitHub release page carrying the binaries, the checksums and
an SBOM. Pushing a `v*` tag runs `.github/workflows/release.yaml`, which builds
the artifacts with GoReleaser and creates a **draft** release. The draft is
never published automatically: a maintainer reviews it and publishes it by
hand.

Everything below is run by a maintainer on their own machine, except where it
says the workflow does it.

## 1. Bump the version

The version is hard coded in `cmd/main.go`:

```go
c.Version = "0.1.0"
```

Set it to the version you are about to tag, without the leading `v`, and commit
that on its own.

## 2. Check the tree

```bash
make lint
make test
make release-snapshot
```

`make release-snapshot` runs GoReleaser with `--snapshot --skip=sbom` and
leaves the four binaries under `dist/`:

```bash
ls dist/*/kubectl-nv
```

The SBOM step is skipped locally because it needs syft, which the release
workflow installs for itself. Everything else, including the archives and the
checksums file, is built exactly as it will be on a tag.

## 3. Tag

Tags are signed:

```bash
git tag -s vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
```

The tag must match the version you put in `cmd/main.go`.

## 4. Review the draft

The workflow creates a draft release on
<https://github.com/NVIDIA/kubectl-nv/releases>. Before publishing, check that:

- the four archives are attached: linux and darwin, amd64 and arm64
- the checksums file and the SBOM are attached
- the generated changelog covers the commits since the previous tag
- the tag name and the version reported by a downloaded binary agree
  (`kubectl-nv --version`)

Downloading one archive and verifying it against the checksums file, the same
way the installation documentation tells users to, is a worthwhile last check.

## 5. Publish

Publish the draft from the release page. That makes the artifacts public and
the page is what `docs/installation.md` points users at.

## Not part of a release yet

There is no krew index for kubectl-nv, and the release does not produce a krew
manifest. The roadmap puts a central krew index submission at v0.5.0; until
then, installation is from the release page or from source.
