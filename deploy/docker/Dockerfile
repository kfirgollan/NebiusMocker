# NebiusMocker all-in-one image: the static linux binary on scratch.
#
# Build with `make docker-image` (cross-compiles bin/linux/nebius-mocker for the
# Docker daemon's architecture, then builds this file with bin/linux as the
# context). Nothing is pulled: there is no base image.
#
# Ports: 4566 gRPC edge (all Nebius services), 4590 registry docker endpoint.
# Running as a container is the documented way to use the docker data plane on
# Docker Desktop, whose daemon can reach published container ports on
# 127.0.0.1 but not processes listening on the host's loopback.
FROM scratch
COPY nebius-mocker /nebius-mocker
EXPOSE 4566 4590
ENTRYPOINT ["/nebius-mocker"]
