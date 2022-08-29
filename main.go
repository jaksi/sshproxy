package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/jaksi/sshutils"
	"golang.org/x/crypto/ssh"
)

const bufferSize = 256

var jsonLogging = flag.Bool("json", false, "enable JSON logging")

type eventSource int

const (
	sClient eventSource = iota
	sServer
)

func (source eventSource) String() string {
	switch source {
	case sClient:
		return "client"
	case sServer:
		return "server"
	}
	panic("unknown event source")
}

type event interface {
	fmt.Stringer
	eventType() string
}

type globalRequestEvent struct {
	Type      string `json:"type"`
	WantReply bool   `json:"want_reply"`
	Payload   string `json:"payload"`
	Accepted  bool   `json:"accepted"`
	Response  string `json:"response"`
}

func (e globalRequestEvent) String() string {
	return fmt.Sprintf("global request: %v, accepted: %v", e.Type, !e.WantReply || e.Accepted)
}

func (globalRequestEvent) eventType() string {
	return "global_request"
}

type closeEvent struct{}

func (closeEvent) String() string {
	return "close"
}

func (closeEvent) eventType() string {
	return "close"
}

type newChannelEvent struct {
	Type          string              `json:"type"`
	Data          string              `json:"data"`
	Accepted      bool                `json:"accepted"`
	ChannelID     string              `json:"channel_id"`
	RejectReason  ssh.RejectionReason `json:"reject_reason"`
	RejectMessage string              `json:"reject_message"`
}

func (e newChannelEvent) String() string {
	if e.Accepted {
		return fmt.Sprintf("new channel: %v, ID: %v", e.Type, e.ChannelID)
	}
	return fmt.Sprintf("new channel: %v, rejected: %v", e.Type, e.RejectMessage)
}

func (newChannelEvent) eventType() string {
	return "new_channel"
}

type channelCloseEvent struct {
	Channel string `json:"channel"`
}

func (e channelCloseEvent) String() string {
	return fmt.Sprintf("channel %v: close", e.Channel)
}

func (channelCloseEvent) eventType() string {
	return "channel_close"
}

type channelEOFEvent struct {
	Channel string `json:"channel"`
}

func (e channelEOFEvent) String() string {
	return fmt.Sprintf("channel %v: EOF", e.Channel)
}

func (channelEOFEvent) eventType() string {
	return "channel_eof"
}

type channelDataEvent struct {
	Channel string `json:"channel"`
	Data    string `json:"data"`
}

func (e channelDataEvent) String() string {
	return fmt.Sprintf("channel %v: stdout: %q", e.Channel, e.Data)
}

func (channelDataEvent) eventType() string {
	return "channel_data"
}

type channelErrorEvent struct {
	Channel string `json:"channel"`
	Data    string `json:"data"`
}

func (e channelErrorEvent) String() string {
	return fmt.Sprintf("channel %v: stderr: %q", e.Channel, e.Data)
}

func (channelErrorEvent) eventType() string {
	return "channel_error"
}

type channelRequestEvent struct {
	Channel   string `json:"channel"`
	Type      string `json:"type"`
	WantReply bool   `json:"want_reply"`
	Payload   string `json:"payload"`
	Accepted  bool   `json:"accepted"`
}

func (e channelRequestEvent) String() string {
	return fmt.Sprintf("channel %v: request: %v, accepted: %v", e.Channel, e.Type, !e.WantReply || e.Accepted)
}

func (channelRequestEvent) eventType() string {
	return "channel_request"
}

func logEvent(source eventSource, e event) {
	if !*jsonLogging {
		log.Printf("%v: %v", source, e)
		return
	}
	msg, err := json.Marshal(struct {
		Type   string `json:"type"`
		Source string `json:"source"`
		Event  event  `json:"event,omitempty"`
	}{
		Type:   e.eventType(),
		Source: source.String(),
		Event:  e,
	})
	if err != nil {
		panic(err)
	}
	log.Print(string(msg))
}

func proxyChannels(client, server *sshutils.Channel) {
	clientEOF := false
	serverEOF := false
	var eofLock sync.Mutex
	var (
		clientWG sync.WaitGroup
		serverWG sync.WaitGroup
	)
	clientWG.Add(1)
	go func() {
		defer clientWG.Done()
		buffer := make([]byte, bufferSize)
		for {
			n, err := client.Read(buffer)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					panic(err)
				}
				eofLock.Lock()
				clientEOF = true
				if !serverEOF {
					if err := server.CloseWrite(); err != nil {
						panic(err)
					}
					logEvent(sClient, channelEOFEvent{Channel: client.ChannelID()})
				}
				eofLock.Unlock()
				break
			}
			if _, err := server.Write(buffer[:n]); err != nil {
				panic(err)
			}
			logEvent(sClient, channelDataEvent{Channel: client.ChannelID(), Data: string(buffer[:n])})
		}
	}()
	serverWG.Add(1)
	go func() {
		defer serverWG.Done()
		buffer := make([]byte, bufferSize)
		for {
			n, err := server.Read(buffer)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					panic(err)
				}
				eofLock.Lock()
				serverEOF = true
				if !clientEOF {
					if err := client.CloseWrite(); err != nil {
						panic(err)
					}
					logEvent(sServer, channelEOFEvent{Channel: server.ChannelID()})
				}
				eofLock.Unlock()
				break
			}
			if _, err := client.Write(buffer[:n]); err != nil {
				panic(err)
			}
			logEvent(sServer, channelDataEvent{Channel: server.ChannelID(), Data: string(buffer[:n])})
		}
	}()
	clientWG.Add(1)
	go func() {
		defer clientWG.Done()
		buffer := make([]byte, bufferSize)
		for {
			n, err := client.Stderr().Read(buffer)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					panic(err)
				}
				break
			}
			if _, err := server.Stderr().Write(buffer[:n]); err != nil {
				panic(err)
			}
			logEvent(sClient, channelErrorEvent{Channel: client.ChannelID(), Data: string(buffer[:n])})
		}
	}()
	serverWG.Add(1)
	go func() {
		defer serverWG.Done()
		buffer := make([]byte, bufferSize)
		for {
			n, err := server.Stderr().Read(buffer)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					panic(err)
				}
				break
			}
			if _, err := client.Stderr().Write(buffer[:n]); err != nil {
				panic(err)
			}
			logEvent(sServer, channelErrorEvent{Channel: server.ChannelID(), Data: string(buffer[:n])})
		}
	}()

	for client.Requests != nil || server.Requests != nil {
		select {
		case request, ok := <-client.Requests:
			if !ok {
				if server.Requests != nil {
					clientWG.Wait()
					if err := server.Close(); err != nil {
						panic(err)
					}
					logEvent(sClient, channelCloseEvent{Channel: client.ChannelID()})
				}
				client.Requests = nil
				continue
			}
			accepted, err := server.RawRequest(request.Type, request.WantReply, request.Payload)
			if err != nil {
				panic(err)
			}
			if err := request.Reply(accepted, nil); err != nil {
				panic(err)
			}
			logEvent(sClient, channelRequestEvent{
				Channel:   client.ChannelID(),
				Type:      request.Type,
				WantReply: request.WantReply,
				Payload:   string(request.Payload),
				Accepted:  accepted,
			})
		case request, ok := <-server.Requests:
			if !ok {
				if client.Requests != nil {
					serverWG.Wait()
					if err := client.Close(); err != nil {
						panic(err)
					}
					logEvent(sServer, channelCloseEvent{Channel: server.ChannelID()})
				}
				server.Requests = nil
				continue
			}
			accepted, err := client.RawRequest(request.Type, request.WantReply, request.Payload)
			if err != nil {
				panic(err)
			}
			if err := request.Reply(accepted, nil); err != nil {
				panic(err)
			}
			logEvent(sServer, channelRequestEvent{
				Channel:   server.ChannelID(),
				Type:      request.Type,
				WantReply: request.WantReply,
				Payload:   string(request.Payload),
				Accepted:  accepted,
			})
		}
	}
}

func proxyConnections(client, server *sshutils.Conn) {
	for client.Requests != nil || client.NewChannels != nil ||
		server.Requests != nil || server.NewChannels != nil {
		select {
		case request, ok := <-client.Requests:
			if !ok {
				if server.Requests != nil {
					if err := server.Close(); err != nil {
						panic(err)
					}
					logEvent(sClient, closeEvent{})
				}
				client.Requests = nil
				continue
			}
			accepted, response, err := server.RawRequest(request.Type, request.WantReply, request.Payload)
			if err != nil {
				panic(err)
			}
			if err := request.Reply(accepted, response); err != nil {
				panic(err)
			}
			logEvent(sClient, globalRequestEvent{
				Type:      request.Type,
				WantReply: request.WantReply,
				Payload:   base64.StdEncoding.EncodeToString(request.Payload),
				Accepted:  accepted,
				Response:  base64.StdEncoding.EncodeToString(response),
			})
		case request, ok := <-server.Requests:
			if !ok {
				if client.Requests != nil {
					if err := client.Close(); err != nil {
						panic(err)
					}
					logEvent(sServer, closeEvent{})
				}
				server.Requests = nil
				continue
			}
			if request.Type == "hostkeys-00@openssh.com" {
				// TODO: spoof that shit
				continue
			}
			accepted, response, err := client.RawRequest(request.Type, request.WantReply, request.Payload)
			if err != nil {
				panic(err)
			}
			if err := request.Reply(accepted, response); err != nil {
				panic(err)
			}
			logEvent(sServer, globalRequestEvent{
				Type:      request.Type,
				WantReply: request.WantReply,
				Payload:   base64.StdEncoding.EncodeToString(request.Payload),
				Accepted:  accepted,
				Response:  base64.StdEncoding.EncodeToString(response),
			})
		case newChannel, ok := <-client.NewChannels:
			if !ok {
				client.NewChannels = nil
				continue
			}
			serverChannel, err := server.RawChannel(newChannel.ChannelType(), newChannel.ExtraData())
			accepted := true
			var channelID string
			var rejectReason ssh.RejectionReason
			var rejectMessage string
			if err != nil {
				var openChannelErr *ssh.OpenChannelError
				if errors.As(err, &openChannelErr) {
					if err := newChannel.Reject(openChannelErr.Reason, openChannelErr.Message); err != nil {
						panic(err)
					}
					accepted = false
					rejectReason = openChannelErr.Reason
					rejectMessage = openChannelErr.Message
				} else {
					panic(err)
				}
			}
			var clientChannel *sshutils.Channel
			if accepted {
				clientChannel, err = newChannel.AcceptChannel()
				if err != nil {
					panic(err)
				}
				if clientChannel.ChannelID() != serverChannel.ChannelID() {
					panic("channel IDs do not match")
				}
				channelID = clientChannel.ChannelID()
			}
			logEvent(sClient, newChannelEvent{
				Type:          newChannel.ChannelType(),
				Data:          base64.StdEncoding.EncodeToString(newChannel.ExtraData()),
				Accepted:      accepted,
				ChannelID:     channelID,
				RejectReason:  rejectReason,
				RejectMessage: rejectMessage,
			})
			if clientChannel != nil {
				go proxyChannels(clientChannel, serverChannel)
			}
		case newChannel, ok := <-server.NewChannels:
			if !ok {
				server.NewChannels = nil
				continue
			}
			clientChannel, err := client.RawChannel(newChannel.ChannelType(), newChannel.ExtraData())
			accepted := true
			var channelID string
			var rejectReason ssh.RejectionReason
			var rejectMessage string
			if err != nil {
				var openChannelErr *ssh.OpenChannelError
				if errors.As(err, &openChannelErr) {
					if err := newChannel.Reject(openChannelErr.Reason, openChannelErr.Message); err != nil {
						panic(err)
					}
					accepted = false
					rejectReason = openChannelErr.Reason
					rejectMessage = openChannelErr.Message
				} else {
					panic(err)
				}
			}
			var serverChannel *sshutils.Channel
			if accepted {
				serverChannel, err = newChannel.AcceptChannel()
				if err != nil {
					panic(err)
				}
				if serverChannel.ChannelID() != clientChannel.ChannelID() {
					panic("channel IDs do not match")
				}
				channelID = serverChannel.ChannelID()
			}
			logEvent(sServer, newChannelEvent{
				Type:          newChannel.ChannelType(),
				Data:          base64.StdEncoding.EncodeToString(newChannel.ExtraData()),
				Accepted:      accepted,
				ChannelID:     channelID,
				RejectReason:  rejectReason,
				RejectMessage: rejectMessage,
			})
			if serverChannel != nil {
				go proxyChannels(clientChannel, serverChannel)
			}
		}
	}
}

func main() {
	listenAddress := flag.String("listen", "", "address to listen on")
	hostKeyFile := flag.String("hostkey", "", "host key file")
	serverAddress := flag.String("connect", "", "address to connect to")
	user := flag.String("user", "", "user to connect as")
	password := flag.String("password", "", "password to connect with")
	keyFile := flag.String("key", "", "key to connect with")
	flag.Parse()

	if *listenAddress == "" || *hostKeyFile == "" || *serverAddress == "" {
		panic("listen, hostkey, and connect are required")
	}

	if *jsonLogging {
		log.SetFlags(0)
	}

	hostKey, err := sshutils.LoadHostKey(*hostKeyFile)
	if err != nil {
		panic(err)
	}
	serverConfig := &ssh.ServerConfig{
		NoClientAuth: true,
	}
	serverConfig.AddHostKey(hostKey)
	listener, err := sshutils.Listen(*listenAddress, serverConfig)
	if err != nil {
		panic(err)
	}
	serverConn, err := listener.Accept()
	if err != nil {
		panic(err)
	}
	listener.Close()

	clientConfig := &ssh.ClientConfig{
		User:            *user,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint: gosec
	}
	if *password != "" {
		clientConfig.Auth = append(clientConfig.Auth, ssh.Password(*password))
	}
	if *keyFile != "" {
		key, err := sshutils.LoadHostKey(*keyFile)
		if err != nil {
			panic(err)
		}
		clientConfig.Auth = append(clientConfig.Auth, ssh.PublicKeys(key))
	}
	clientConn, err := sshutils.Dial(*serverAddress, clientConfig)
	if err != nil {
		panic(err)
	}

	proxyConnections(serverConn, clientConn)
}
