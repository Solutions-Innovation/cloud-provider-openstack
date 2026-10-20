# Tool image for converting OVA/OVF/VMDK disk images to RAW and importing
# them onto an OpenStack Cinder volume (exposed as a raw block device via
# the Cinder CSI driver's Block Volume mode). Used by job.yaml.
#
# Uses qemu-img for all conversion (default mode writes directly to the
# target block device via `qemu-img convert -n --target-is-zero`, skipping
# any intermediate RAW file; see convert-and-import.sh for details and the
# "qemu-img" fallback mode for targets that don't read back as zero).
# virt-v2v was evaluated but dropped: its output modes (local file,
# oVirt/RHV, OpenStack-via-REST-API) have no way to write directly to an
# arbitrary pre-existing block device, so it could not support the
# space-saving direct-write mode that is now the default here.
FROM registry.fedoraproject.org/fedora-minimal:44

# Build-time proxy support (e.g. --build-arg http_proxy=http://proxy:3128/).
# Declared as ARGs so they are available to RUN instructions below; not
# baked into the final image's ENV, so running containers are unaffected.
ARG http_proxy
ARG https_proxy
ARG no_proxy

WORKDIR /root

RUN microdnf install -y --nodocs --setopt install_weak_deps=False \
        bash \
        curl ca-certificates tar unzip \
        qemu-img \
        util-linux \
        coreutils \
        python3 \
        perl-Digest-SHA && \
    rm -rf /var/cache/dnf /var/tmp/*

COPY convert-and-import.sh /scripts/convert-and-import.sh
RUN chmod +x /scripts/convert-and-import.sh

ENTRYPOINT ["/bin/bash", "/scripts/convert-and-import.sh"]
