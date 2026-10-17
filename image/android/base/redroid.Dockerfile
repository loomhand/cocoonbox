# The Android of the public Android box: AOSP redroid 16 (Launcher3), flattened to one layer for the VM's vfs image
# store. It carries no Google apps.
FROM redroid/redroid:16.0.0_64only-latest@sha256:7b1e389bd15f37af3bcd06138f5b2ffa7cfba4332bd5ef54c53e99c2f160a15b AS redroid

FROM --platform=$BUILDPLATFORM ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac AS cleaned
COPY --from=redroid / /redroid/
RUN rm -rf /redroid/system/product/app/Camera2 /redroid/system/product/app/Browser2 && \
    test ! -e /redroid/system/product/priv-app/PrebuiltGmsCore

FROM scratch
COPY --from=cleaned /redroid/ /
ENTRYPOINT ["/init", "qemu=1", "androidboot.hardware=redroid"]
