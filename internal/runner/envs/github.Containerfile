# offload/github: GitHub Actions runners. The actions runner's .NET dependencies, plus rootless
# podman for `docker` in workflows. Inner containers share the job container's namespaces and
# cgroup (its limits already apply); builds use chroot isolation.
FROM offload/base
RUN apt-get update && apt-get install -y --no-install-recommends \
      podman podman-docker buildah crun fuse-overlayfs uidmap \
      libicu74 libkrb5-3 liblttng-ust1t64 zlib1g \
    && rm -rf /var/lib/apt/lists/* && touch /etc/containers/nodocker
RUN printf '%s\n' '[containers]' 'netns="host"' 'userns="host"' 'ipcns="host"' 'utsns="host"' \
      'cgroupns="host"' 'cgroups="disabled"' 'log_driver="k8s-file"' \
      '[engine]' 'cgroup_manager="cgroupfs"' 'events_logger="file"' 'runtime="crun"' \
      > /etc/containers/containers.conf
ENV BUILDAH_ISOLATION=chroot RUNNER_ALLOW_RUNASROOT=1
