package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
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

	payload sshutils.Payload
}

func (e globalRequestEvent) String() string {
	return fmt.Sprintf("global request: %s", e.payload)
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

	payload sshutils.Payload
}

func (e newChannelEvent) String() string {
	return fmt.Sprintf("new channel %v: %s", e.ChannelID, e.payload)
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

	payload sshutils.Payload
}

func (e channelRequestEvent) String() string {
	return fmt.Sprintf("channel %v: request: %s", e.Channel, e.payload)
}

func (channelRequestEvent) eventType() string {
	return "channel_request"
}

type entry struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Event  event  `json:"event"`
}

var entries = []entry{}

func logEvent(source eventSource, e event) {
	if !*jsonLogging {
		log.Printf("%v: %v", source, e)
	} else {
		entries = append(entries, entry{
			Type:   e.eventType(),
			Source: source.String(),
			Event:  e,
		})
	}
}

type channelContext struct {
	*sshutils.Channel
	stdout, stderr sync.WaitGroup
	eofLock        *sync.Mutex
	eof            bool
}

func proxyChannelStdout(local, remote *channelContext, source eventSource) {
	local.stdout.Add(1)
	go func() {
		defer local.stdout.Done()
		buffer := make([]byte, bufferSize)
		for {
			n, err := local.Read(buffer)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					panic(err)
				}
				local.eofLock.Lock()
				local.eof = true
				if !remote.eof {
					local.stderr.Wait()
					if err := remote.CloseWrite(); err != nil {
						panic(err)
					}
					logEvent(source, channelEOFEvent{Channel: local.ChannelID()})
				}
				local.eofLock.Unlock()
				break
			}
			if _, err := remote.Write(buffer[:n]); err != nil {
				panic(err)
			}
			logEvent(source, channelDataEvent{Channel: local.ChannelID(), Data: string(buffer[:n])})
		}
	}()
}

func proxyChannelStderr(local, remote *channelContext, source eventSource) {
	local.stderr.Add(1)
	go func() {
		defer local.stderr.Done()
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
	}()
}

func proxyChannelRequest(request *sshutils.ChannelRequest, remote *sshutils.Channel, source eventSource) {
	payload, err := request.UnmarshalPayload()
	if err != nil {
		panic(err)
	}
	accepted, err := remote.Request(request.Type, request.WantReply, payload)
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

		payload: payload,
	})
}

func proxyChannels(client, server *sshutils.Channel) {
	var eof sync.Mutex
	clientContext := channelContext{client, sync.WaitGroup{}, sync.WaitGroup{}, &eof, false}
	serverContext := channelContext{server, sync.WaitGroup{}, sync.WaitGroup{}, &eof, false}

	proxyChannelStdout(&clientContext, &serverContext, sClient)
	proxyChannelStdout(&serverContext, &clientContext, sServer)

	proxyChannelStderr(&clientContext, &serverContext, sClient)
	proxyChannelStderr(&serverContext, &clientContext, sServer)

	for client.Requests != nil || server.Requests != nil {
		select {
		case request, ok := <-client.Requests:
			if !ok {
				if server.Requests != nil {
					clientContext.stdout.Wait()
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
					serverContext.stdout.Wait()
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

var (
	hostKeys         []*sshutils.HostKey
	serverPublicKeys sshutils.PublicKeys
)

func proxyGlobalRequest(request *sshutils.GlobalRequest, local, remote *sshutils.Conn, source eventSource) {
	payload, err := request.UnmarshalPayload()
	if err != nil {
		panic(err)
	}
	var response []byte
	requestPayload := payload
	switch payload := payload.(type) {
	case *sshutils.HostkeysRequestPayload:
		serverPublicKeys = payload.Hostkeys

		publicKeys := make(sshutils.PublicKeys, len(hostKeys))
		for i, hostKey := range hostKeys {
			publicKeys[i] = hostKey.PublicKey()
		}
		requestPayload = &sshutils.HostkeysRequestPayload{
			Hostkeys: publicKeys,
		}
	case *sshutils.HostkeysProveRequestPayload:
		responseHostKeys := make([]*sshutils.HostKey, len(payload.Hostkeys))
		for i, publicKey := range payload.Hostkeys {
			for _, hostKey := range hostKeys {
				if bytes.Equal(hostKey.PublicKey().Marshal(), publicKey.Marshal()) {
					responseHostKeys[i] = hostKey
					break
				}
			}
			if responseHostKeys[i] == nil {
				panic("host key not found")
			}
		}
		response, err = payload.Response(rand.Reader, responseHostKeys, local.SessionID())
		if err != nil {
			panic(err)
		}

		requestPayload = &sshutils.HostkeysProveRequestPayload{
			Hostkeys: serverPublicKeys,
		}
	}
	accepted, requestResponse, err := remote.Request(request.Type, request.WantReply, requestPayload)
	if err != nil {
		panic(err)
	}
	if requestPayload, ok := requestPayload.(*sshutils.HostkeysProveRequestPayload); ok {
		if err := requestPayload.VerifyResponse(requestResponse, remote.SessionID()); err != nil {
			panic(err)
		}
	}
	if response == nil {
		response = requestResponse
	}
	if err := request.Reply(accepted, response); err != nil {
		if err.Error() != "ssh: disconnect, reason 11: disconnected by user" {
			// Remote sent a request and then disconnected before we could reply.
			panic(err)
		}
	}
	logEvent(source, globalRequestEvent{
		Type:      request.Type,
		WantReply: request.WantReply,
		Payload:   hex.EncodeToString(request.Payload),
		Accepted:  accepted,
		Response:  hex.EncodeToString(requestResponse),

		payload: payload,
	})
}

func proxyNewChannel(newChannel *sshutils.NewChannel, remote *sshutils.Conn, source eventSource) {
	payload, err := newChannel.UnmarshalPayload()
	if err != nil {
		panic(err)
	}
	remoteChannel, err := remote.Channel(newChannel.ChannelType(), payload)
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
		Data:          hex.EncodeToString(newChannel.ExtraData()),
		Accepted:      accepted,
		ChannelID:     channelID,
		RejectReason:  rejectReason,
		RejectMessage: rejectMessage,

		payload: payload,
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
			proxyGlobalRequest(request, client, server, sClient)
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
			proxyGlobalRequest(request, server, client, sServer)
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
	hostKeyFiles := flag.String("hostkeys", "", "host key files (comma-separated)")
	serverAddress := flag.String("connect", "", "address to connect to")
	user := flag.String("user", "", "user to connect as")
	password := flag.String("password", "", "password to connect with")
	keyFile := flag.String("key", "", "key to connect with")
	flag.Parse()

	if *listenAddress == "" || *hostKeyFiles == "" || *serverAddress == "" {
		panic("listen, hostkey, and connect are required")
	}

	log.SetOutput(os.Stdout)
	if *jsonLogging {
		log.SetFlags(0)
	}

	conn, err := net.Dial("tcp", *serverAddress)
	if err != nil {
		panic(err)
	}
	reader := bufio.NewReader(conn)
	serverVersion, err := reader.ReadBytes('\r')
	if err != nil {
		panic(err)
	}
	serverVersion = serverVersion[:len(serverVersion)-1]
	if err := conn.Close(); err != nil {
		panic(err)
	}

	serverConfig := &ssh.ServerConfig{
		NoClientAuth:  true,
		ServerVersion: string(serverVersion),
	}

	hostKeyFileNames := strings.Split(*hostKeyFiles, ",")
	hostKeys = make([]*sshutils.HostKey, len(hostKeyFileNames))
	for i, hostKeyFileName := range hostKeyFileNames {
		hostKey, err := sshutils.LoadHostKey(hostKeyFileName)
		if err != nil {
			panic(err)
		}
		hostKeys[i] = hostKey
		serverConfig.AddHostKey(hostKey)
	}
	listener, err := sshutils.Listen(*listenAddress, serverConfig)
	if err != nil {
		panic(err)
	}
	serverConn, err := listener.Accept()
	if err != nil {
		panic(err)
	}
	if err := listener.Close(); err != nil {
		panic(err)
	}

	clientConfig := &ssh.ClientConfig{
		User:            *user,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint: gosec
		ClientVersion:   string(serverConn.ClientVersion()),
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

	if *jsonLogging {
		msg, err := json.MarshalIndent(struct {
			Client  string  `json:"client"`
			Server  string  `json:"server"`
			User    string  `json:"user"`
			Entries []entry `json:"entries"`
		}{
			Client:  string(clientConn.ClientVersion()),
			Server:  string(serverConn.ServerVersion()),
			User:    *user,
			Entries: entries,
		}, "", "  ")
		if err != nil {
			panic(err)
		}
		log.Print(string(msg))
	}
}
