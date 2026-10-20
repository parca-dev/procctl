# procctl-testagent: cross-compiled static Go binary.
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -o /procctl-testagent ./cmd/procctl-testagent

# Runs as root: it needs CAP_SYS_PTRACE (pidfd_getfd, process_vm_readv) and,
# for fast discovery, CAP_BPF/CAP_PERFMON, granted by the DaemonSet.
FROM gcr.io/distroless/static-debian12
COPY --from=build /procctl-testagent /procctl-testagent
ENTRYPOINT ["/procctl-testagent"]
