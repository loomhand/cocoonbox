# Box image

The box is a desktop template on the sandboxd host: `cbcore:vN`. The older `cbdesk:vN` family is frozen: its boxes keep the agent their image carries, and `cut.sh` refuses it. This page is the build, versioning and publishing flow.

## Recipe

| Piece | Where |
|---|---|
| guest provisioning (Xorg, XFCE, Chrome, users, units) | `image/provision.sh`, `image/rootfs/` |
| the desk runtime of `cbcore` (desk API, `gui_plan`, routing) | `image/desk/` |
| the agent | the `cocoonbox-agent` binary of the same release |
| cut, seal, export | `image/cut.sh`, `image/seal.sh`, `image/export-rootfs.sh` |
| guest gate | `image/clean-gate.sh` |

## Cut

A version is cut from the previous one on a host that runs the box's sandboxd instance (`deploy/cbsbx/`):

```bash
sudo image/cut.sh -f cbcore -a cocoonbox-agent-linux-amd64 [-p] cbcore:v(N-1) vN
```

`cut.sh` boots the base template, reruns `provision.sh` with `-p`, installs the agent, runs the guest gate, seals, stops, exports a flat rootfs tarball with `export-rootfs.sh` and imports it as `cbcore:vN` with `cocoon image import`. The tarball stays under the instance's `build/` directory.

A runtime is a directory with a `provision.sh`; each `-r <dir>` copies one into the guest and runs its `provision.sh` after the base step, in the order given, and the family, `cbcore` unless `-f` names another, uses `image/desk` without `-r`. `-f` takes any family name (`[a-z][a-z0-9]*`): `-f <family> -r image/desk -r <dir>` cuts `<family>:vN` from a template with the desk runtime plus another one, such as a directory outside this repository. The guest gate runs for `cbcore` only. `-n` prints the steps a cut would run, in order, and changes nothing; `make image` (in the gate workflow) checks that list with `image/test/cut-dry-run.sh`: the `-r` order, the `cbcore` default and the refusals, `cbdesk` among them. cua-driver comes with the shuttle archive as shuttle's child; the desk provision removes a template's own cua-driver and masks its unit, which keeps a restored home's wants link inert.

## Versions and pins

- `<family>:vN` counts up per family and is never re-cut: a fix is a new N. `cbcore:vN` is the template name on the sandboxd host; the published image is `base` (below).
- A release tag `vX.Y.Z` of this repository publishes the agent (linux amd64 and arm64) and api binaries, `CocoonBox-macos-arm64.zip` when the repository variable `CB_ENDPOINT` is set, and `cocoonbox-image-recipe-vX.Y.Z.tar.gz`, the `image/` tree of that commit with `PINS` naming the sha256 of both agent binaries, the shuttle archive's version and sha256, and silkd's version and the sha256 of its two builds. An image cut from a release uses that release's agent, so the pins name exactly what the image carries.
- Before the tag, the candidate api binary passes `cocoonbox-api -config <api.yaml> check` against a copy of each live api.yaml, and the release notes name an upgrade step for each change under `api/deploy/` or `deploy/host/` since the last tag (a unit, install-pg, install-api), which `check` does not see. A `check` failure blocks the tag; such a change is an upgrade step the notes carry. An image's box acceptance runs against the api deployed on its host, so an api upgrade gets its own acceptance there.
- A new image becomes a pool default only after a full boot and the box acceptance on a host.
- `image/silkd.sh` pins silkd as sandbox's own guest images carry it: the silkd carrier image `ghcr.io/cocoonstack/sandbox/silkd` by version and digest, and the sha256 of its `/silkd` (glibc, which `cbcore` takes) and `/silkd-static` (which `cbdroid` takes). `image/silkd.sh fetch image/desk/vendor` packs both next to the shuttle archive; a cut checks the build it installs against its pin.

## Publishing

cocoonstack publishes guest images as OCI images on ghcr (`ghcr.io/cocoonstack/...`, built from Dockerfiles by Actions, such as `sandbox/base` with an immutable `24.04-<sha>` and a moving `24.04`). The box is cut from a booted VM instead, so its publish step runs where the cut runs:

- an accepted `cbcore:vN` is published as `ghcr.io/loomhand/cocoonbox/base:vN`, an OCI artifact (`oras push`, one layer of type `application/vnd.loomhand.cocoonbox.rootfs.tar+gzip`), and `latest` moves to it;
- `vN` is immutable: it is pushed once, never re-tagged, and stays available for a rollback; the workflow refuses a `vN` that already exists, unless it carries this run's agent, in which case a re-run only moves `latest`;
- `latest` always names the newest accepted `vN`;
- the manifest's annotations pin the agent, shuttle and silkd the image carries:

  | Annotation | Value |
  |---|---|
  | `org.opencontainers.image.revision` | the release commit the image was cut from |
  | `org.opencontainers.image.source` | this repository |
  | `io.github.loomhand.cocoonbox.agent-sha256` | sha256 of the `cocoonbox-agent` binary in the image |
  | `io.github.loomhand.cocoonbox.agent-rev` | the revision after `+` in the agent's version (`0.1.0+<rev>`) |
  | `io.github.loomhand.cocoonbox.shuttle-version` | the [loomhand/shuttle](https://github.com/loomhand/shuttle) archive the desk daemon and runtime come from |
  | `io.github.loomhand.cocoonbox.shuttle-sha256` | sha256 of that linux-x64 archive |
  | `io.github.loomhand.cocoonbox.silkd-version` | the [cocoonstack/sandbox](https://github.com/cocoonstack/sandbox) silkd the image carries |
  | `io.github.loomhand.cocoonbox.silkd-sha256` | sha256 of that `/usr/local/bin/silkd` |

- a host imports a version as its template: `oras pull ghcr.io/loomhand/cocoonbox/base:vN` and `cocoon image import cbcore:vN cocoonbox-cbcore-vN.tar.gz`;
- `.github/workflows/image.yml` cuts and publishes on a self-hosted runner labelled `cocoon` on the sandboxd host, installed by `deploy/runner/install`:
  - the runner's job-started hook (`deploy/runner/job-started.sh`) fails every job before its first step unless it is CMGS's dispatch of `image.yml` on main, so no pull request reaches the host;
  - the runner user may run only `cb-image-cut` with sudo; it cuts from a root-owned tree of the release tag, which must be on main, with the release's agent checked against `checksums.txt`, and refuses a `cbcore:vN` that exists or a cbsbx headroom under 35G;
  - the cut takes the vendor tarballs, shuttle's archive and silkd's, from `/var/lib/cb-image/vendor` on the host; when a pin moves, refresh them on a machine with docker with `image/desk/shuttle.sh fetch <dir>` and `image/silkd.sh fetch <dir>` and copy them there, since the host has no docker and a cut refuses to start without them;
  - the `cut` job leaves `cbcore:vN` and its tarball for the box acceptance on that host; the `publish` job waits for an approval in the `ghcr-publish` environment, checks the tarball's agent against the release again and pushes with the job's `GITHUB_TOKEN` through a registry config under `RUNNER_TEMP`.
