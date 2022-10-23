# sshproxy
SSH proxy, primarily intended to be used to create replay tests for [sshesame](https://github.com/jaksi/sshesame)

Displays detailed information about global requests, new channels, channel requests, channel data (stdout and stderr), channel EOF, channel closure and connection closure events from both the client and the server.

```
docker build -t sshproxy .
docker run -itp 127.0.0.1:2022:2022 --rm sshproxy | tee test-$(date +%s).json
# Execute test case against 127.0.0.1:2022 (ssh -p 2022 127.0.0.1)
```
