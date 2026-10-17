# The Android of the public Android box: AOSP redroid 16 (Launcher3) with the bake wrapper, flattened to one layer for
# the VM's vfs image store. It carries no Google apps.
FROM redroid/redroid:16.0.0_64only-latest@sha256:7b1e389bd15f37af3bcd06138f5b2ffa7cfba4332bd5ef54c53e99c2f160a15b AS redroid

# a static BusyBox runs the bake wrapper before Android's /init has mounted APEX
FROM busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94 AS tools

FROM --platform=$BUILDPLATFORM ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac AS overlay
COPY bake-init.sh /output/cocoon-bake-init
COPY bake-once /output/.cocoon-bake-once
COPY --from=tools /bin/busybox /output/busybox
RUN chmod 0755 /output/cocoon-bake-init

FROM redroid AS merged
COPY --from=overlay /output/ /

FROM --platform=$BUILDPLATFORM ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac AS cleaned
COPY --from=merged / /redroid/
RUN rm -rf /redroid/system/product/app/Camera2 /redroid/system/product/app/Browser2 && \
    test -x /redroid/cocoon-bake-init && test -x /redroid/busybox && test -f /redroid/.cocoon-bake-once && \
    test ! -e /redroid/system/product/priv-app/PrebuiltGmsCore

FROM scratch
COPY --from=cleaned /redroid/ /
WORKDIR /
ENTRYPOINT ["/busybox", "sh", "/cocoon-bake-init", "qemu=1", "androidboot.hardware=redroid"]
