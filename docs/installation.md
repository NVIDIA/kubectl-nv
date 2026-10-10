# Installation

## From the GitHub release page

Every release publishes a `tar.gz` archive per operating system and
architecture, a checksums file and an SBOM, on the
[release page](https://github.com/NVIDIA/kubectl-nv/releases).

Pick the archive that matches your machine:

| Operating system | Architecture | Archive |
|---|---|---|
| Linux | x86_64 | `kubectl-nv_<version>_linux_amd64.tar.gz` |
| Linux | arm64 | `kubectl-nv_<version>_linux_arm64.tar.gz` |
| macOS | Intel | `kubectl-nv_<version>_darwin_amd64.tar.gz` |
| macOS | Apple silicon | `kubectl-nv_<version>_darwin_arm64.tar.gz` |

If you are not sure which architecture you have, run `uname -sm`.

### Download and verify

Download the archive and the `kubectl-nv_<version>_checksums.txt` file into the
same directory, then check the archive against it:

```bash
VERSION=0.1.0
OS=linux          # or darwin
ARCH=amd64        # or arm64

curl -fsSLO "https://github.com/NVIDIA/kubectl-nv/releases/download/v${VERSION}/kubectl-nv_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "https://github.com/NVIDIA/kubectl-nv/releases/download/v${VERSION}/kubectl-nv_${VERSION}_checksums.txt"

shasum -a 256 -c kubectl-nv_${VERSION}_checksums.txt --ignore-missing
```

The line for your archive must read `OK`. On Linux, `sha256sum -c` takes the
same arguments.

### Put it on your PATH

kubectl finds a plugin by looking for an executable named `kubectl-nv` on your
`PATH`, which is what makes `kubectl nv` work:

```bash
tar -xzf kubectl-nv_${VERSION}_${OS}_${ARCH}.tar.gz kubectl-nv
chmod +x kubectl-nv
sudo install -m 0755 kubectl-nv /usr/local/bin/kubectl-nv
```

If you would rather not use `sudo`, move it into any directory that is already
on your `PATH`, for example `~/.local/bin`.

### Confirm

```bash
kubectl plugin list
```

`kubectl-nv` appears in the output. Then:

```bash
kubectl nv --version
kubectl nv adm must-gather --help
```

On macOS the first run may be blocked by Gatekeeper because the binary is not
notarized. Clear the quarantine attribute with
`xattr -d com.apple.quarantine /usr/local/bin/kubectl-nv`.

## From source

```bash
git clone https://github.com/NVIDIA/kubectl-nv.git
cd kubectl-nv
make build
./bin/kubectl-nv --help
```

Copy `bin/kubectl-nv` onto your `PATH` as above to use it through kubectl.

## Prerequisites

- `kubectl`, and a kubeconfig for the cluster you want to inspect
- Go, only for building from source. The version is the one in
  `deployments/devel/Dockerfile`

## Krew

kubectl-nv is not in a krew index yet. Install it from the release page or from
source until it is.
