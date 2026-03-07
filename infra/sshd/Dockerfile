FROM alpine:3.21

RUN apk add --no-cache openssh-server && \
    mkdir -p /run/sshd /root/.ssh && \
    chmod 700 /root/.ssh

COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 22

ENTRYPOINT ["/entrypoint.sh"]
