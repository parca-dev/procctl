# procctl-testclient: static (musl) build of testclient/testclient.c.
# Built per target platform (buildx runs the build stage under emulation for
# foreign architectures; it is one small C file).
FROM alpine:3.20 AS build
RUN apk add --no-cache gcc musl-dev linux-headers
WORKDIR /src
COPY c/procctl.h c/procctl.h
COPY testclient/ testclient/
RUN gcc -std=gnu11 -O2 -Wall -Werror -static -Ic -Itestclient \
      -o /procctl-testclient testclient/testclient.c -lpthread

FROM scratch
COPY --from=build /procctl-testclient /procctl-testclient
USER 65532:65532
ENTRYPOINT ["/procctl-testclient"]
