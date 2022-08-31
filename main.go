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

type channelContext struct {
	*sshutils.Channel
	wg  sync.WaitGroup
	eof *sync.Mutex
}

func proxyChannelStdouts(client, server *channelContext) {
	clientEOF := false
	serverEOF := false

	client.wg.Add(1)
	go func() {
		defer client.wg.Done()
		buffer := make([]byte, bufferSize)
		for {
			n, err := client.Read(buffer)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					panic(err)
				}
				client.eof.Lock()
				clientEOF = true
				if !serverEOF {
					if err := server.CloseWrite(); err != nil {
						panic(err)
					}
					logEvent(sClient, channelEOFEvent{Channel: client.ChannelID()})
				}
				client.eof.Unlock()
				break
			}
			if _, err := server.Write(buffer[:n]); err != nil {
				panic(err)
			}
			logEvent(sClient, channelDataEvent{Channel: client.ChannelID(), Data: string(buffer[:n])})
		}
	}()

	server.wg.Add(1)
	go func() {
		defer server.wg.Done()
		buffer := make([]byte, bufferSize)
		for {
			n, err := server.Read(buffer)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					panic(err)
				}
				client.eof.Lock()
				serverEOF = true
				if !clientEOF {
					if err := client.CloseWrite(); err != nil {
						panic(err)
					}
					logEvent(sServer, channelEOFEvent{Channel: server.ChannelID()})
				}
				client.eof.Unlock()
				break
			}
			if _, err := client.Write(buffer[:n]); err != nil {
				panic(err)
			}
			logEvent(sServer, channelDataEvent{Channel: server.ChannelID(), Data: string(buffer[:n])})
		}
	}()
}

func proxyChannelStderr(local, remote *sshutils.Channel, source eventSource) {
	buffer := make([]byte, bufferSize)
	for {
		n, err := local.Stderr().Read(buffer)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				panic(err)
			}
			break
		}
		if _, err := remote.Stderr().Write(buffer[:n]); err != nil {
			panic(err)
		}
		logEvent(source, channelErrorEvent{Channel: local.ChannelID(), Data: string(buffer[:n])})
	}
}

func proxyChannelStderrs(client, server *channelContext) {
	client.wg.Add(1)
	go func() {
		proxyChannelStderr(client.Channel, server.Channel, sClient)
		client.wg.Done()
	}()

	server.wg.Add(1)
	go func() {
		proxyChannelStderr(server.Channel, client.Channel, sServer)
		server.wg.Done()
	}()
}

func proxyChannelRequest(request *ssh.Request, remote *sshutils.Channel, source eventSource) {
	accepted, err := remote.RawRequest(request.Type, request.WantReply, request.Payload)
	if err != nil {
		panic(err)
	}
	if err := request.Reply(accepted, nil); err != nil {
		panic(err)
	}
	logEvent(source, channelRequestEvent{
		Channel:   remote.ChannelID(),
		Type:      request.Type,
		WantReply: request.WantReply,
		Payload:   string(request.Payload),
		Accepted:  accepted,
	})
}

func proxyChannels(client, server *sshutils.Channel) {
	var eof sync.Mutex
	clientContext := channelContext{client, sync.WaitGroup{}, &eof}
	serverContext := channelContext{server, sync.WaitGroup{}, &eof}

	proxyChannelStdouts(&clientContext, &serverContext)

	proxyChannelStderrs(&clientContext, &serverContext)

	for client.Requests != nil || server.Requests != nil {
		select {
		case request, ok := <-client.Requests:
			if !ok {
				if server.Requests != nil {
					clientContext.wg.Wait()
					if err := server.Close(); err != nil {
						panic(err)
					}
					logEvent(sClient, channelCloseEvent{Channel: client.ChannelID()})
				}
				client.Requests = nil
				continue
			}
			proxyChannelRequest(request, server, sClient)
		case request, ok := <-server.Requests:
			if !ok {
				if client.Requests != nil {
					serverContext.wg.Wait()
					if err := client.Close(); err != nil {
						panic(err)
					}
					logEvent(sServer, channelCloseEvent{Channel: server.ChannelID()})
				}
				server.Requests = nil
				continue
			}
			proxyChannelRequest(request, client, sServer)
		}
	}
}

func proxyGlobalRequest(request *ssh.Request, remote *sshutils.Conn, source eventSource) {
	accepted, response, err := remote.RawRequest(request.Type, request.WantReply, request.Payload)
	if err != nil {
		panic(err)
	}
	if err := request.Reply(accepted, response); err != nil {
		panic(err)
	}
	logEvent(source, globalRequestEvent{
		Type:      request.Type,
		WantReply: request.WantReply,
		Payload:   base64.StdEncoding.EncodeToString(request.Payload),
		Accepted:  accepted,
		Response:  base64.StdEncoding.EncodeToString(response),
	})
}

func proxyNewChannel(newChannel *sshutils.NewChannel, remote *sshutils.Conn, source eventSource) {
	remoteChannel, err := remote.RawChannel(newChannel.ChannelType(), newChannel.ExtraData())
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
	var localChannel *sshutils.Channel
	if accepted {
		localChannel, err = newChannel.AcceptChannel()
		if err != nil {
			panic(err)
		}
		if localChannel.ChannelID() != remoteChannel.ChannelID() {
			panic("channel IDs do not match")
		}
		channelID = localChannel.ChannelID()
	}
	logEvent(source, newChannelEvent{
		Type:          newChannel.ChannelType(),
		Data:          base64.StdEncoding.EncodeToString(newChannel.ExtraData()),
		Accepted:      accepted,
		ChannelID:     channelID,
		RejectReason:  rejectReason,
		RejectMessage: rejectMessage,
	})
	if localChannel != nil {
		go proxyChannels(localChannel, remoteChannel)
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
			proxyGlobalRequest(request, server, sClient)
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
			proxyGlobalRequest(request, client, sServer)
		case newChannel, ok := <-client.NewChannels:
			if !ok {
				client.NewChannels = nil
				continue
			}
			proxyNewChannel(newChannel, server, sClient)
		case newChannel, ok := <-server.NewChannels:
			if !ok {
				server.NewChannels = nil
				continue
			}
			proxyNewChannel(newChannel, client, sServer)
		}
	}
}

func main() {
	listenAddress := flag.String("listen", "", "address to listen on")
	hostKeyFile := flag.String("hostkey", "", "host key file")
	serverVersion := flag.String("server-version", "SSH-2.0-OpenSSH_9.0", "server version")
	serverAddress := flag.String("connect", "", "address to connect to")
	clientVersion := flag.String("client-version", "SSH-2.0-OpenSSH_9.0", "client version")
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
		NoClientAuth:  true,
		ServerVersion: *serverVersion,
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
		ClientVersion:   *clientVersion,
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
