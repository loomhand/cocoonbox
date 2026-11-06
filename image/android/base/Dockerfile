# Android box base: cocoon's Ubuntu 24.04 with docker, the baked redroid container, scrcpy-rfb, ashmem and the desk runtime; build context image/.

FROM ghcr.io/cocoonstack/cocoon/ubuntu:24.04@sha256:54572ca079710227271eb7210e88a48e476d1ce286f2a3ebb733d4162f66e9e2
ARG TARGETARCH
ADD android/base/docker-data-${TARGETARCH}.tar /var/lib/docker/
COPY android/provision.sh /tmp/cb-image/android/provision.sh
COPY android/rootfs /tmp/cb-image/android/rootfs
COPY android/ashmem /tmp/cb-image/android/ashmem
COPY android/base/verify.sh /tmp/cb-image/android/base/verify.sh
COPY desk/shuttle.sh /tmp/cb-image/desk/shuttle.sh
COPY desk/rootfs/usr/local/bin/desk /tmp/cb-image/desk/rootfs/usr/local/bin/desk
COPY desk/rootfs/usr/local/lib/cocoonbox/shuttle-config /tmp/cb-image/desk/rootfs/usr/local/lib/cocoonbox/shuttle-config
COPY desk/service/cocoonbox.json /tmp/cb-image/desk/service/cocoonbox.json
RUN bash /tmp/cb-image/android/provision.sh && \
    bash /tmp/cb-image/android/base/verify.sh && \
    rm -rf /tmp/cb-image /var/lib/apt/lists/* && \
    printf 'cbdroid base %s\n' "$(date -u +%FT%TZ)" > /etc/cocoonbox/android-base

CMD ["/sbin/init"]
