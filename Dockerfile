FROM alpine:latest as builder
ARG TARGETPLATFORM
RUN echo "I'm building for $TARGETPLATFORM"

RUN apk add --no-cache gzip && \
    mkdir /clashauto-config && \
    wget -O /clashauto-config/geoip.metadb https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb && \
    wget -O /clashauto-config/geosite.dat https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat && \
    wget -O /clashauto-config/geoip.dat https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.dat

COPY docker/file-name.sh /clashauto/file-name.sh
WORKDIR /clashauto
COPY bin/ bin/
RUN FILE_NAME=`sh file-name.sh` && echo $FILE_NAME && \
    FILE_NAME=`ls bin/ | egrep "$FILE_NAME.gz"|awk NR==1` && echo $FILE_NAME && \
    mv bin/$FILE_NAME clashauto.gz && gzip -d clashauto.gz && chmod +x clashauto && echo "$FILE_NAME" > /clashauto-config/test
FROM alpine:latest
LABEL org.opencontainers.image.source="https://github.com/ClashrAuto/clash"

RUN apk add --no-cache ca-certificates tzdata iptables

VOLUME ["/root/.config/clashauto/"]

COPY --from=builder /clashauto-config/ /root/.config/clashauto/
COPY --from=builder /clashauto/clashauto /clashauto
ENTRYPOINT [ "/clashauto" ]
