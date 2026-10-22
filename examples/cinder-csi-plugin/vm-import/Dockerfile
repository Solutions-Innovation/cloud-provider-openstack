# Tool image for converting OVA/OVF/VMDK disk images to RAW and importing
# them onto an OpenStack Cinder volume (exposed as a raw block device via
# the Cinder CSI driver's Block Volume mode). Used by job.yaml.
#
# qemu-img handles the default/fast paths (CONVERT_MODE=qemu-img-direct
# writes directly to the target block device via `qemu-img convert -n
# --target-is-zero`, skipping any intermediate RAW file; CONVERT_MODE=
# qemu-img is the fallback for targets that don't read back as zero).
#
# virt-v2v + libguestfs are included for CONVERT_MODE=virt-v2v, which is
# REQUIRED for Windows guests: qemu-img is a pure format converter and
# never touches anything inside the guest filesystem, so a Windows VM
# exported from VMware (VMware-specific drivers, boot configuration tied
# to VMware's virtual hardware) will typically fail to boot under
# KVM/QEMU without driver injection and boot-config fixups, which only
# virt-v2v performs. virt-v2v has no way to write directly to an
# arbitrary pre-existing block device (its output modes target local
# files, oVirt/RHV, or OpenStack-via-REST-API), so this mode converts to
# a scratch RAW file and dd's it onto the target, like CONVERT_MODE=
# qemu-img -- it does not support the space-saving direct-write
# optimization that qemu-img-direct uses for Linux sources. See
# convert-and-import.sh and OPERATIONS-GUIDE.md Section 3 for the full
# comparison and when each mode is required vs. optional.
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
        qemu-img qemu-kvm qemu-tools nbd \
        libguestfs libguestfs-tools-c virt-v2v \
        util-linux util-linux-script \
        coreutils \
        python3 \
        perl-Digest-SHA && \
    rm -rf /var/cache/dnf /var/tmp/*

COPY convert-and-import.sh /scripts/convert-and-import.sh
RUN chmod +x /scripts/convert-and-import.sh

ENV LIBGUESTFS_BACKEND=direct

ENTRYPOINT ["/bin/bash", "/scripts/convert-and-import.sh"]
