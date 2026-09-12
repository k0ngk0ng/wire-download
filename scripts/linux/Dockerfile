FROM golang:1.26.2-bookworm
RUN sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/debian.sources \
    && apt-get update && apt-get install -y --no-install-recommends \
    build-essential autoconf automake libtool pkg-config \
    curl ca-certificates patch xz-utils bzip2 python3 \
    libssl-dev zlib1g-dev patchelf
WORKDIR /work/wire-download
CMD ["sh", "scripts/release.sh"]
