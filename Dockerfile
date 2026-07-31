FROM alpine:latest as builder
ARG TARGETPLATFORM
RUN echo "I'm building for $TARGETPLATFORM"

RUN apk add --no-cache gzip && \
    mkdir /coast-config && \
    wget -O /coast-config/geoip.metadb https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb && \
    wget -O /coast-config/geosite.dat https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat && \
    wget -O /coast-config/geoip.dat https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.dat

COPY docker/file-name.sh /coast/file-name.sh
WORKDIR /coast
COPY bin/ bin/
RUN FILE_NAME=`sh file-name.sh` && echo $FILE_NAME && \
    FILE_NAME=`ls bin/ | egrep "$FILE_NAME.gz"|awk NR==1` && echo $FILE_NAME && \
    mv bin/$FILE_NAME coast.gz && gzip -d coast.gz && chmod +x coast && echo "$FILE_NAME" > /coast-config/test
FROM alpine:latest
LABEL org.opencontainers.image.source="https://github.com/ClashrAuto/coast"

RUN apk add --no-cache ca-certificates tzdata iptables

VOLUME ["/root/.config/coast/"]

COPY --from=builder /coast-config/ /root/.config/coast/
COPY --from=builder /coast/coast /coast
ENTRYPOINT [ "/coast" ]
