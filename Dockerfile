FROM golang as build-env
WORKDIR /go/src/sshproxy
ADD . /go/src/sshproxy
RUN go build -o /go/bin/sshproxy
FROM debian
COPY --from=build-env /go/bin/sshproxy /
RUN apt update && apt install -y openssh-server openssl
RUN useradd -m -s /bin/bash user -p "$(openssl passwd -1 hunter2)"
RUN mkdir /run/sshd
EXPOSE 2022
CMD /usr/sbin/sshd && /sshproxy -listen :2022 -hostkey /etc/ssh/ssh_host_ed25519_key -connect localhost:22 -user user -password hunter2 -json
