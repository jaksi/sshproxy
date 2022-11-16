FROM golang
RUN apt update; apt install -y openssh-server openssl
RUN useradd -m -s /bin/bash user -p "$(openssl passwd -1 hunter2)"
RUN mkdir /run/sshd
ADD . /go/src/sshproxy
WORKDIR /go/src/sshproxy
RUN go build -o /go/bin/sshproxy
EXPOSE 2022
CMD /usr/sbin/sshd && /go/bin/sshproxy -listen :2022 -hostkeys /etc/ssh/ssh_host_rsa_key,/etc/ssh/ssh_host_ecdsa_key,/etc/ssh/ssh_host_ed25519_key -connect localhost:22 -user user -password hunter2 -json
