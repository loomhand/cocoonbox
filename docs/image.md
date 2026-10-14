# Box image

The box is a desktop template on the sandboxd host: `cbdesk:vN` (frozen) and `cbcore:vN` (current). This page is the build, versioning and publishing flow.

## Recipe

| Piece | Where |
|---|---|
| guest provisioning (Xorg, XFCE, Chrome, users, units) | `image/provision.sh`, `image/rootfs/` |
| the desk runtime of `cbcore` (desk API, `gui_plan`, routing) | `image/desk/` |
| cua-driver, built from a pinned commit of [loomhand/cua](https://github.com/loomhand/cua) | `image/cua-driver.sh` (`COMMIT`, `VERSION`) |
| the agent | the `cocoonbox-agent` binary of the same release |
| cut, seal, export | `image/cut.sh`, `image/seal.sh`, `image/export-rootfs.sh` |
| guest gate | `image/clean-gate.sh` |

## Cut

A version is cut from the previous one on a host that runs the box's sandboxd instance (`deploy/cbsbx/`):

```bash
sudo image/cut.sh -f cbcore -a cocoonbox-agent-linux-amd64 [-p] cbcore:v(N-1) vN
```

`cut.sh` boots the base template, reruns `provision.sh` with `-p`, installs the agent, runs the guest gate, seals, stops, exports a flat rootfs tarball with `export-rootfs.sh` and imports it as `cbcore:vN` with `cocoon image import`. The tarball stays under the instance's `build/` directory.

## Versions and pins

- `<family>:vN` counts up per family and is never re-cut: a fix is a new N. `cbcore:vN` is the template name on the sandboxd host; the published image is `base` (below).
- A release tag `vX.Y.Z` of this repository publishes the agent and api binaries and `cocoonbox-image-recipe-vX.Y.Z.tar.gz`, the `image/` tree of that commit with `PINS` naming the agent binary's sha256 and the cua-driver commit and version. An image cut from a release uses that release's agent, so the pins name exactly what the image carries.
- A new image becomes a pool default only after a full boot and the box acceptance on a host.

## Publishing

cocoonstack publishes guest images as OCI images on ghcr (`ghcr.io/cocoonstack/...`, built from Dockerfiles by Actions, such as `sandbox/base` with an immutable `24.04-<sha>` and a moving `24.04`). The box is cut from a booted VM instead, so its publish step runs where the cut runs:

- an accepted `cbcore:vN` is published as `ghcr.io/loomhand/cocoonbox/base:vN`, an OCI artifact (`oras push`, one layer of type `application/vnd.loomhand.cocoonbox.rootfs.tar+gzip`), and `latest` moves to it;
- `vN` is immutable: it is pushed once, never re-tagged, and stays available for a rollback; the workflow refuses a `vN` that already exists;
- `latest` always names the newest accepted `vN`;
- the manifest's annotations pin what the image carries:

  | Annotation | Value |
  |---|---|
  | `org.opencontainers.image.revision` | the release commit the image was cut from |
  | `org.opencontainers.image.source` | this repository |
  | `io.github.loomhand.cocoonbox.agent-sha256` | sha256 of the `cocoonbox-agent` binary in the image |
  | `io.github.loomhand.cocoonbox.agent-rev` | the revision the agent reports (`0.1.0+<rev>`) |
  | `io.github.loomhand.cocoonbox.cua-driver-version` | the cua-driver build version |
  | `io.github.loomhand.cocoonbox.cua-driver-commit` | the [loomhand/cua](https://github.com/loomhand/cua) commit cua-driver is built from |

- a host imports a version as its template: `oras pull ghcr.io/loomhand/cocoonbox/base:vN` and `cocoon image import cbcore:vN cocoonbox-cbcore-vN.tar.gz`;
- `.github/workflows/image.yml` cuts and publishes. It needs a self-hosted runner labelled `cocoon` on a sandboxd host; until one is registered the workflow does not run.
